# 前端产物与 CPU 架构无关：在构建机原生平台编译，避免 QEMU 模拟 arm64 时 yarn 卡死
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/yarn.lock ./
RUN yarn config set network-timeout 600000 \
	&& yarn install --frozen-lockfile
COPY frontend/ .
RUN yarn build

# Go 交叉编译：同样在原生平台执行，按目标架构产出二进制（前端已 embed）
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS backend
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ .
COPY --from=frontend /src/dist/ ./internal/webui/dist/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath \
	-ldflags="-s -w -X github.com/livehl/mirrorhub/internal/version.Version=${VERSION}" \
	-o /out/mirrorhub ./cmd/mirrorhub

# 运行（单进程：下载服务 + 管理台 + 内置 SQLite）
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
	&& mkdir -p /data
WORKDIR /app
COPY --from=backend /out/mirrorhub /app/mirrorhub

ENV MIRRORHUB_DATA_DIR=/data \
	MIRRORHUB_PROXY_ADDR=:18081 \
	MIRRORHUB_ADMIN_ADDR=:18082 \
	MIRRORHUB_LOG_LEVEL=info

VOLUME ["/data"]
EXPOSE 18081 18082
ENTRYPOINT ["/app/mirrorhub"]
