# syntax=docker/dockerfile:1

# demoapp：KubeStasis 演示负载（暴露 /metrics 的最小 HTTP 服务）。
# 与主镜像同一套约定：多阶段 + distroless + 非 root。
#
# 运行时基础镜像可覆盖（ARG 必须在任何 FROM 之前声明才对 FROM 生效）：
#   --build-arg RUNTIME_IMAGE=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM golang:1.22 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/demoapp/ cmd/demoapp/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOPROXY=${GOPROXY} \
    go build -trimpath -ldflags="-s -w" -o /demoapp ./cmd/demoapp

FROM ${RUNTIME_IMAGE}
COPY --from=builder /demoapp /demoapp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/demoapp"]
