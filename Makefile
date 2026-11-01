# KubeSentry Makefile —— kubebuilder v4 风格
# 用法：make help

IMG ?= kubesentry:dev
GO  ?= go

##@ General
help: ## 显示帮助
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

##@ Development
manifests: ## 生成 CRD / RBAC / Webhook YAML（controller-gen）
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 \
		paths=./api/... paths=./internal/... \
		crd:allowDangerousTypes=true \
		rbac:roleName=manager-role \
		webhook \
		output:crd:artifacts:config=config/crd/bases

generate: ## 生成 DeepCopy 等样板代码
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 object:headerFile="hack/boilerplate.go.txt" paths=./api/... paths=./internal/...

fmt: ## gofmt + goimports
	$(GO) fmt ./...
	$(GO) run golang.org/x/tools/cmd/goimports@latest -w .

lint: ## golangci-lint
	golangci-lint run ./...

run: ## 本地运行（连当前 kubeconfig）
	$(GO) run ./cmd

##@ Test
test: manifests generate fmt ## 单元 + envtest 集成测试
	$(GO) test ./... -coverprofile=coverage.out -covermode=atomic
	$(GO) tool cover -func=coverage.out | tail -1

##@ Build
build: ## 编译二进制
	$(GO) build -o bin/kubesentry ./cmd

docker-build: ## 构建镜像（多阶段 + distroless）
	docker build -t $(IMG) .

docker-push: ## 推送镜像
	docker push $(IMG)

##@ Deploy
install: ## 安装 CRD
	kubectl apply -f config/crd/bases

deploy: ## 部署 Operator（RBAC + Webhook + Manager）
	kubectl apply -k config/default

undeploy: ## 卸载
	kubectl delete -k config/default

##@ Chaos（本地 kind 混沌验证）
chaos-kill: ## 随机杀 Pod，验证自愈
	kubectl delete pod -l app.kubernetes.io/managed-by=kubesentry --field-selector=status.phase=Running

.PHONY: help manifests generate fmt lint run test build docker-build docker-push install deploy undeploy chaos-kill
