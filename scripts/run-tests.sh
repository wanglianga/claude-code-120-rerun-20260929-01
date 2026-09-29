#!/usr/bin/env bash
# 一键运行集成测试。
# 用法：
#   ./scripts/run-tests.sh                 # 自动探测：优先 docker compose，否则使用本机 PG/Redis
#   TEST_PG_DSN=... TEST_REDIS_ADDR=... ./scripts/run-tests.sh   # 显式指定
set -euo pipefail
cd "$(dirname "$0")/.."

GO_BIN="${GO_BIN:-}"
pick_go() {
  for cand in "$GO_BIN" go /home/node/go122/bin/go /usr/local/go/bin/go; do
    [ -x "$cand" ] || command -v "$cand" >/dev/null 2>&1 || continue
    ver=$("$cand" version 2>/dev/null | grep -oE 'go1\.[0-9]+' | head -1 | grep -oE '[0-9]+$')
    if [ -n "$ver" ] && [ "$ver" -ge 22 ]; then echo "$cand"; return 0; fi
  done
  return 1
}
GO_BIN="$(pick_go)" || { echo "未找到 Go 1.22+，请设置 GO_BIN"; exit 1; }
echo ">> 使用 Go: $($GO_BIN version)"

if [ -z "${TEST_PG_DSN:-}" ] || [ -z "${TEST_REDIS_ADDR:-}" ]; then
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    echo ">> 使用 docker compose 准备 PostgreSQL/Redis 测试环境"
    docker compose -f docker-compose.yml up -d postgres redis
    # 等待就绪
    for i in $(seq 1 40); do
      docker compose exec -T postgres pg_isready -U seafood >/dev/null 2>&1 && break
      sleep 2
    done
    export TEST_PG_DSN="host=127.0.0.1 port=5432 user=seafood password=seafood dbname=seafood_test sslmode=disable"
    export TEST_REDIS_ADDR="127.0.0.1:6379"
    docker compose exec -T postgres psql -U seafood -d postgres -tc \
      "SELECT 1 FROM pg_database WHERE datname='seafood_test'" | grep -q 1 \
      || docker compose exec -T postgres createdb -U seafood seafood_test
    docker compose exec -T redis redis-cli -n 1 flushdb >/dev/null || true
    CLEANUP=1
  else
    # 回退：使用本机/用户态 PG 与 Redis（本仓库开发环境）
    PGBIN=""; [ -x /usr/lib/postgresql/15/bin/psql ] && PGBIN=/usr/lib/postgresql/15/bin
    PGHOST_RUN="${PGHOST_RUN:-/home/node/pgrun}"; PGPORT_RUN="${PGPORT_RUN:-5433}"; PGUSER_RUN="${PGUSER_RUN:-seafood}"
    "$PGBIN/dropdb" -h "$PGHOST_RUN" -p "$PGPORT_RUN" -U "$PGUSER_RUN" seafood_test 2>/dev/null || true
    "$PGBIN/createdb" -h "$PGHOST_RUN" -p "$PGPORT_RUN" -U "$PGUSER_RUN" seafood_test
    export TEST_PG_DSN="host=$PGHOST_RUN port=$PGPORT_RUN user=$PGUSER_RUN dbname=seafood_test sslmode=disable"
    export TEST_REDIS_ADDR="${TEST_REDIS_ADDR:-localhost:6379}"
    redis-cli -n 1 flushdb >/dev/null || true
  fi
fi

echo ">> TEST_PG_DSN=$TEST_PG_DSN"
echo ">> 运行 go test（端到端集成测试 + RBAC）"
GOTOOLCHAIN=local GOPROXY=https://goproxy.cn,direct "$GO_BIN" test -v .
