// main_test.go
package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// 测试 extractTimestamp
func TestExtractTimestamp(t *testing.T) {
	tests := []struct {
		tag      string
		expected string
	}{
		{"dev_20250806213400", "20250806213400"},
		{"release_20250806203400", "20250806203400"},
		{"v1.2.3", ""},
		{"test_20250101", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractTimestamp(tt.tag)
		if got != tt.expected {
			t.Errorf("extractTimestamp(%q) = %q, want %q", tt.tag, got, tt.expected)
		}
	}
}

// 测试 extractTag
func TestExtractTag(t *testing.T) {
	tests := []struct {
		img      string
		expected string
	}{
		{"nginx:dev_20250806213400", "dev_20250806213400"},
		{"nginx:1.2.3", "1.2.3"},
		{"nginx", ""},
		{"repo/nginx:release_20250806203400", "release_20250806203400"},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractTag(tt.img)
		if got != tt.expected {
			t.Errorf("extractTag(%q) = %q, want %q", tt.img, got, tt.expected)
		}
	}
}

// 测试 isSemverRollback
func TestIsSemverRollback(t *testing.T) {
	tests := []struct {
		oldImg   string
		newImg   string
		expected bool
	}{
		{"nginx:1.2.3", "nginx:1.2.2", true},
		{"nginx:1.2.3", "nginx:1.2.4", false},
		{"nginx:dev_20250806213400", "nginx:dev_20250806203400", true}, // 非semver，走时间戳比较
		{"nginx:1.2.3", "nginx:1.2.3", false},
	}
	for _, tt := range tests {
		got := isSemverRollback(tt.oldImg, tt.newImg)
		if got != tt.expected {
			t.Errorf("isSemverRollback(%q, %q) = %v, want %v", tt.oldImg, tt.newImg, got, tt.expected)
		}
	}
}

// 测试 getTagCompareRegexps
func TestGetTagCompareRegexps(t *testing.T) {
	client := fake.NewSimpleClientset()
	// 创建一个模拟的 ConfigMap
	_, err := client.CoreV1().ConfigMaps("default").Create(
		context.TODO(),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tag-validation-webhook-config",
				Namespace: "default",
			},
			Data: map[string]string{
				"lower-tag-regexp":   "dev_.*",
				"greater-tag-regexp": "release_.*",
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("failed to create fake ConfigMap: %v", err)
	}
	lower, greater := getTagCompareRegexps(client)
	if lower != "dev_.*" || greater != "release_.*" {
		t.Errorf("getTagCompareRegexps = (%q, %q), want (\"dev_.*\", \"release_.*\")", lower, greater)
	}
}

// 测试 webhookHandler.isImageRollback
func TestIsImageRollback(t *testing.T) {
	// 构造 fake clientset 和 handler
	client := fake.NewSimpleClientset()
	// 创建ConfigMap，模拟自定义规则
	client.CoreV1().ConfigMaps("default").Create(
		context.TODO(),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tag-validation-webhook-config",
				Namespace: "default",
			},
			Data: map[string]string{
				"lower-tag-regexp":   "dev_.*",
				"greater-tag-regexp": "release_.*",
			},
		},
		metav1.CreateOptions{},
	)
	h := &webhookHandler{client: client}

	tests := []struct {
		oldImg   string
		newImg   string
		expected bool
	}{
		// 自定义规则命中
		{"nginx:dev_20250806213400", "nginx:release_20250806203400", true},
		// 时间戳比较
		{"nginx:dev_20250806213400", "nginx:dev_20250806203400", true},
		{"nginx:dev_20250806203400", "nginx:dev_20250806213400", false},
		// semver比较
		{"nginx:1.2.3", "nginx:1.2.2", true},
		{"nginx:1.2.3", "nginx:1.2.4", false},
		// 普通字符串比较
		{"nginx:abc", "nginx:aaa", true},
	}
	for _, tt := range tests {
		got := h.isImageRollback(tt.oldImg, tt.newImg)
		if got != tt.expected {
			t.Errorf("isImageRollback(%q, %q) = %v, want %v", tt.oldImg, tt.newImg, got, tt.expected)
		}
	}
}
