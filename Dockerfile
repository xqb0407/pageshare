# syntax=docker/dockerfile:1
# pageshare 单二进制镜像：管理 UI 已 go:embed 进二进制（internal/server/web/dist），
# 全部依赖纯 Go（modernc SQLite / aws-sdk），CGO 可关——最终镜像只有一个 alpine + 二进制。

FROM golang:1.26-alpine AS build
WORKDIR /src
# 版本号由 CI 传 --build-arg VERSION=<tag>；与 scripts/release.sh 注入同一个 main.version
ARG VERSION=dev
# 先只拷依赖清单，源码改动不击穿 module 缓存层
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pageshare ./cmd/pageshare

FROM alpine:3.20
# ca 证书：出网请求 S3/R2 必需；tzdata：日志/过期时间本地化
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 pageshare \
 && mkdir -p /data && chown pageshare:pageshare /data
COPY --from=build /out/pageshare /usr/local/bin/pageshare
USER pageshare
# 元数据（db_path）与审计（audit.jsonl）都落 /data；named volume 首挂会继承此目录属主
VOLUME /data
EXPOSE 8300
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -q --spider http://127.0.0.1:8300/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/pageshare"]
