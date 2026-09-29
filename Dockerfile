# ---- 构建阶段 ----
FROM golang:1.22-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# lib/pq 与 go-redis 均为纯 Go，可关闭 CGO 产出静态二进制
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seafood-app .

# ---- 运行阶段 ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl && \
    adduser -D -u 10001 app
WORKDIR /app
COPY --from=builder /out/seafood-app /app/seafood-app
COPY --from=builder /src/migrations /app/migrations
USER app
EXPOSE 8080
ENV HTTP_ADDR=:8080 \
    MIGRATIONS_DIR=/app/migrations
HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=10 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/seafood-app"]
