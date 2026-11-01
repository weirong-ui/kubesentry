# KubeSentry 多阶段构建：builder 编译 → distroless 运行
# 安全卖点：无 shell、无包管理器、非 root —— CKS 面试可以直接聊
FROM golang:1.22 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w" -o kubesentry ./cmd

# distroless/base-nonroot：无 root、无 shell，攻击面最小
FROM gcr.io/distroless/base-debian12:nonroot
WORKDIR /
COPY --from=builder /workspace/kubesentry .
USER 65532:65532

ENTRYPOINT ["/kubesentry"]
