// main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"go.uber.org/zap/zapcore"
	"gomodules.xyz/jsonpatch/v2"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	admission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type webhookHandler struct {
	decoder admission.Decoder
	client  kubernetes.Interface
}

func (h *webhookHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	// ctrllog.Log.V(1).Info("收到请求", "用户", req.UserInfo.Username)
	if !h.mutationEnabledFromConfigMap() {
		return admission.Allowed("mutation disabled by ConfigMap")
	}
	if !slices.Contains(watchedNamespaces, req.Namespace) && watchNamespace != "all" {
		ctrllog.Log.V(1).Info("not in watch namespace", "reqNamespace", req.Namespace)
		return admission.Allowed("not in watch namespace")
	}
	if req.Kind.Kind != "Deployment" && req.Kind.Kind != "StatefulSet" {
		ctrllog.Log.V(1).Info("not deployment/statefulset")
		return admission.Allowed("not deployment/statefulset")
	}

	var oldImg, newImg string
	var isHelmAction bool

	switch req.Kind.Kind {
	case "Deployment":
		oldObj := &appsv1.Deployment{}
		newObj := &appsv1.Deployment{}
		if err := h.decoder.DecodeRaw(req.OldObject, oldObj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if err := h.decoder.Decode(req, newObj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if len(oldObj.Spec.Template.Spec.Containers) > 0 {
			oldImg = oldObj.Spec.Template.Spec.Containers[0].Image
		}
		if len(newObj.Spec.Template.Spec.Containers) > 0 {
			newImg = newObj.Spec.Template.Spec.Containers[0].Image
		}
		ts := newObj.GetAnnotations()["helm.sh/timestamp"]
		if ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				if time.Since(parsed) < 60*time.Second {
					isHelmAction = true
				} else {
					ctrllog.Log.V(1).Info("not a helm upgrade action, bypass this request", "deploy", newObj.GetName(), "oldImg", oldImg, "newImg", newImg)
				}
			}
		} else {
			ctrllog.Log.V(1).Info("annotation helm.sh/timestamp not found, bypass this request", "deploy", newObj.GetName(), "oldImg", oldImg, "newImg", newImg)
		}

	case "StatefulSet":
		oldObj := &appsv1.StatefulSet{}
		newObj := &appsv1.StatefulSet{}
		if err := h.decoder.DecodeRaw(req.OldObject, oldObj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if err := h.decoder.Decode(req, newObj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if len(oldObj.Spec.Template.Spec.Containers) > 0 {
			oldImg = oldObj.Spec.Template.Spec.Containers[0].Image
		}
		if len(newObj.Spec.Template.Spec.Containers) > 0 {
			newImg = newObj.Spec.Template.Spec.Containers[0].Image
		}
		ts := newObj.GetAnnotations()["helm.sh/timestamp"]
		if ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				if time.Since(parsed) < 60*time.Second {
					isHelmAction = true
				} else {
					ctrllog.Log.V(1).Info("not a helm upgrade action, bypass this request", "sts", newObj.GetName(), "oldImg", oldImg, "newImg", newImg)
				}
			}
		} else {
			ctrllog.Log.V(1).Info("annotation helm.sh/timestamp not found, bypass this request", "sts", newObj.GetName(), "oldImg", oldImg, "newImg", newImg)
		}
	}
	if !isHelmAction {
		return admission.Allowed("not a helm upgrade action, bypass this request")
	}

	// log.Printf("received image request from %s", req.UserInfo.Username)

	if h.isImageRollback(oldImg, newImg) {
		patchStr := fmt.Sprintf(`[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"%s"}]`, oldImg)
		var patchOps []jsonpatch.JsonPatchOperation
		if err := json.Unmarshal([]byte(patchStr), &patchOps); err == nil {
			ctrllog.Log.V(1).Info("image rollback blocked", "holdWithOldImage", true, "oldImg", oldImg, "newImg", newImg)
			return admission.Patched("image rollback blocked", patchOps...)
		} else {
			ctrllog.Log.V(1).Info("failed to create patch", "oldImg", oldImg, "newImg", newImg)
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to create patch: %w", err))
		}
	}
	ctrllog.Log.V(1).Info("image updated", "oldImg", oldImg, "newImg", newImg)
	return admission.Allowed("image updated")
}

// 提取tag中的时间戳（如 dev_20250806213400），返回时间戳字符串，失败返回空
func extractTimestamp(tag string) string {
	re := regexp.MustCompile(`(\d{14})`)
	match := re.FindStringSubmatch(tag)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

// 读取ConfigMap中的自定义tag比较规则
func getTagCompareRegexps(client kubernetes.Interface) (string, string) {
	cm, err := client.CoreV1().ConfigMaps("default").Get(context.TODO(), "tag-validation-webhook-config", metav1.GetOptions{})
	if err != nil {
		return "", ""
	}
	return cm.Data["lower-tag-regexp"], cm.Data["greater-tag-regexp"]
}

// 判断镜像tag是否回滚，优先级：自定义规则 > 时间戳 > semver > 普通字符串
func (h *webhookHandler) isImageRollback(oldImg, newImg string) bool {
	oldTag := extractTag(oldImg)
	newTag := extractTag(newImg)

	// 1. 自定义正则规则
	lowerRe, greaterRe := getTagCompareRegexps(h.client)
	if lowerRe != "" && greaterRe != "" {
		lowerMatch, _ := regexp.MatchString(lowerRe, oldTag)
		greaterMatch, _ := regexp.MatchString(greaterRe, newTag)
		if lowerMatch && greaterMatch {
			ctrllog.Log.V(1).Info("matched rule: custom regexp", "lowerMatch", lowerMatch, "greaterMatch", greaterMatch)
			return true // old < new，视为回滚
		}
	}

	// 2. 时间戳比较
	oldTs := extractTimestamp(oldTag)
	newTs := extractTimestamp(newTag)
	if oldTs != "" && newTs != "" {
		ctrllog.Log.V(1).Info("matched rule: timestamp in name", "oldTs", oldTs, "newTs", newTs)
		return newTs < oldTs
	}

	// 3. semver 比较
	if isSemverRollback(oldImg, newImg) {
		ctrllog.Log.V(1).Info("matched rule: semver", "oldImg", oldImg, "newImg", newImg)
		return true
	}

	// 4. 普通字符串比较
	ctrllog.Log.V(1).Info("matched rule: fallback - ascii", "oldTag", oldTag, "newTag", newTag)
	return newTag < oldTag
}

/*
原 isImageRollback 已被 webhookHandler 的方法替代
*/

func isSemverRollback(oldImg, newImg string) bool {
	oldTag := extractTag(oldImg)
	newTag := extractTag(newImg)
	oldVer, errOld := semver.NewVersion(oldTag)
	newVer, errNew := semver.NewVersion(newTag)
	if errOld != nil || errNew != nil {
		return newTag < oldTag
	}
	return newVer.LessThan(oldVer)
}

func extractTag(img string) string {
	for i := len(img) - 1; i >= 0; i-- {
		if img[i] == ':' {
			return img[i+1:]
		}
	}
	return ""
}

func (h *webhookHandler) mutationEnabledFromConfigMap() bool {
	cm, err := h.client.CoreV1().ConfigMaps("default").Get(context.TODO(), "tag-validation-webhook-config", metav1.GetOptions{})
	if err != nil {
		log.Printf("failed to read ConfigMap: %v", err)
		return false
	}
	if cm.Data["enableMutation"] == "true" {
		return true
	}
	return false
}

var (
	certFile          string
	keyFile           string
	kubeconfig        string
	watchNamespace    string
	watchedNamespaces []string
	logLevel          string
)

func init() {
	flag.StringVar(&certFile, "tls-cert-file", "/certs/cert", "TLS cert")
	flag.StringVar(&keyFile, "tls-key-file", "/certs/key", "TLS key")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&watchNamespace, "namespaced", "all", "Namespace to watch. Use 'all' for cluster scope.")
	flag.StringVar(&logLevel, "log-level", "info", "日志级别，可选: debug, info, warn, error")
}
func getKubeClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
func main() {
	flag.Parse()
	watchedNamespaces = strings.Split(watchNamespace, ",")

	var zapLevel zapcore.Level
	if logLevel == "debug" {
		zapLevel = zapcore.DebugLevel
	} else {
		zapLevel = zapcore.InfoLevel
	}
	zapOpts := zap.Options{
		Development: logLevel == "debug",
		Level:       zapLevel,
	}
	ctrllog.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	decoder := admission.NewDecoder(scheme.Scheme)
	clientset, err := getKubeClient(kubeconfig)
	if err != nil {
		log.Fatalf("Failed to init kube client: %v", err)
	}
	ctrllog.Log.V(1).Info("已建立 k8s client 连接")
	h := &webhookHandler{
		decoder: decoder,
		client:  clientset,
	}
	http.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		server := admission.Webhook{Handler: h}
		server.ServeHTTP(w, r)
	})

	log.Printf("Starting server on 8443 with cert: %s", certFile)
	if err := http.ListenAndServeTLS(":8443", certFile, keyFile, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
