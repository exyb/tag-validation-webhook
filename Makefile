APP_NAME=tag-validation-webhook
CERT_DIR=certs
NAMESPACE=default
TAG := $(shell date +%Y%m%d%H%M%S)

build:
	go build -o bin/${APP_NAME} main.go
run:
	go run main.go --tls-cert-file=$(CERT_DIR)/cert --tls-private-key-file=$(CERT_DIR)/key

docker-build:
	docker build --no-cache -t harbor.asiainfo.com/public/${APP_NAME}:dev_${TAG} .
docker-push: docker-build
	docker push harbor.asiainfo.com/public/${APP_NAME}:dev_${TAG}

apply-manifests:
	kubectl apply -f manifests/

port-forward:
	kubectl port-forward svc/${APP_NAME} 8443:443 -n ${NAMESPACE}
