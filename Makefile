# KubeStasis Makefile —— kubebuilder v4 风格
# 用法：make help

IMG ?= kubestasis:dev
GO  ?= go
KUBECTL ?= kubectl
# 显式列出包路径：controller-gen v0.16.5 在 Go 1.22+ 的模块模式下对 `./...` 解析不稳，
# 会报 "no Go files in .../api"，这里直接写死目录，可复现。
PKG_API := ./api/v1alpha1
PKG_PATHS := paths=./api/v1alpha1 paths=./internal/controller paths=./internal/scaler paths=./internal/healer paths=./internal/webhook
CONTROLLER_GEN := $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

##@ General
help: ## 显示帮助
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

##@ Development
manifests: ## 生成 CRD / RBAC YAML（controller-gen）
	$(CONTROLLER_GEN) \
		$(PKG_PATHS) \
		crd:allowDangerousTypes=true \
		rbac:roleName=manager-role \
		webhook \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook

generate: ## 生成 DeepCopy 等样板代码
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths=$(PKG_API) paths=./internal/...

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
	$(GO) build -o bin/kubestasis ./cmd

docker-build: ## 构建镜像（多阶段 + distroless）
	docker build -t $(IMG) .

docker-push: ## 推送镜像
	docker push $(IMG)

##@ Deploy
install: ## 安装 CRD
	$(KUBECTL) apply -k config/crd

# 不需要外部 kustomize 二进制：kubectl 自带，IMG 用占位符替换
MANIFEST = $(KUBECTL) kustomize config/default | sed 's|IMG_PLACEHOLDER|$(IMG)|g'

deploy: manifests ## 一键部署 CRD + RBAC + Manager（干净集群可用）
	$(MANIFEST) | $(KUBECTL) apply -f -

# 带 Admission Webhook 的形态：overlay 开启 --enable-webhook 并挂载证书，
# 自签证书 + caBundle 注入由 hack/webhook-certs.sh 完成（不依赖 cert-manager）
MANIFEST_WEBHOOK = $(KUBECTL) kustomize config/webhook-enabled | sed 's|IMG_PLACEHOLDER|$(IMG)|g'

deploy-webhook: manifests ## 部署 + 启用 Admission Webhook（自签证书一条龙）
	$(MANIFEST_WEBHOOK) | $(KUBECTL) apply -f -
	bash hack/webhook-certs.sh
	$(KUBECTL) -n kubestasis-system rollout restart deploy/kubestasis-controller-manager
	$(KUBECTL) -n kubestasis-system rollout status deploy/kubestasis-controller-manager --timeout=180s

undeploy: ## 卸载（含 webhook 形态）
	$(MANIFEST_WEBHOOK) | $(KUBECTL) delete -f - --ignore-not-found
	$(MANIFEST) | $(KUBECTL) delete -f - --ignore-not-found

##@ Chaos（本地 kind 混沌验证）
chaos-kill: ## 随机杀 Pod，验证自愈
	kubectl delete pod -l app.kubernetes.io/managed-by=kubestasis --field-selector=status.phase=Running

.PHONY: help manifests generate fmt lint run test build docker-build docker-push install deploy deploy-webhook undeploy chaos-kill
