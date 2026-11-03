# syntax=docker/dockerfile:1
# KubeStasis 多阶段构建：builder 编译 → distroless 运行
# 安全卖点：无 shell、无包管理器、非 root —— CKS 面试可以直接聊

# 全局 ARG 必须声明在第一个 FROM 之前，才能用于 FROM 指令。
# 国内拉不到 gcr.io 时：--build-arg RUNTIME_IMAGE=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM golang:1.22 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
# 国内构建可用 --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /workspace

# 先只拷依赖清单，让 go mod download 这一层可被缓存
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
# pkg/ 随 P4（手写 mini-Informer）落地后再加进来

# CGO_ENABLED=0：产物是纯静态二进制，才能塞进 distroless/static
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o kubestasis ./cmd

# distroless/static-nonroot：无 shell、无包管理器、默认非 root，攻击面最小
FROM ${RUNTIME_IMAGE}
WORKDIR /
COPY --from=builder /workspace/kubestasis .
USER 65532:65532

ENTRYPOINT ["/kubestasis"]
