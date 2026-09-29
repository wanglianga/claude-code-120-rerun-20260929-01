# ---- 构建阶段 ----
FROM golang:1.22-bookworm AS builder

# 国内构建可使用加速代理：--build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} GO111MODULE=on CGO_ENABLED=0

WORKDIR /build

# 先拉依赖以利用构建缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码（含内嵌的 migrations / openapi.yaml / web）
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/seafood-trace .

# ---- 运行阶段 ----
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r app && useradd -r -g app app

WORKDIR /app
COPY --from=builder /out/seafood-trace /app/seafood-trace

USER app
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=5 \
  CMD curl -fsS http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/seafood-trace"]
