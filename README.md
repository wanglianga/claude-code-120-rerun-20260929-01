# 海鲜预制菜工厂 · 批次解冻加工与召回服务

面向虾仁、扇贝、鱿鱼、鱼柳冻品的**全链路批次追溯与精准召回**服务（Go + PostgreSQL + Redis）。
从采购入库、仓储温控、解冻、清洗/切配/调味/称重/包装/金检/留样/入成品冷库，到渠道定制包装发货、
已售订单、渠道反馈、监管抽样，再到召回定位、停售退货材料、退货三态处置与供应商扣款/客户赔付，
数据全部关联到**原料批次与成品批次**，召回时可精准到**箱**和**订单**。

## 一、能力清单

- **采购入库**：供应商、捕捞/生产日期、入库温度、检疫证明、箱号、库位；入库温度超上限自动冻结批次（ON_HOLD）并开质量事件。
- **仓储温控**：巡检温度记录，每次超标都开质量事件；品控驳回可解除冻结。
- **多产线并行**：不同产线可同时在制；生产计划可指定预期调味料批次。
- **解冻管理**：解冻开始/结束时间、中心温度、用水批次、操作人；超时或中心温度超标自动开质量事件；冻结原料禁止解冻。
- **加工工序**：清洗/切配/调味/称重/包装/金检/留样/入冷库；**调味料批次用错、金检报警、包装漏气**自动开质量事件。
- **成品与谱系**：完工生成成品批次、成品箱、原料→成品双向谱系、自动留样，转入成品冷库。
- **留样与监管抽样**：留样实验室结论（FAIL 自动开事件）；监管抽样 FAIL 自动生成已确认的 `REGULATOR_RECALL` 事件。
- **渠道定制包装**：通用/餐饮/电商/直播等包装规格，发货可整单改贴渠道包装。
- **冷库与盘点**：按库位/状态/区域查箱、库存汇总、冷库盘点（账实差异以实盘回写，盘亏留痕）。
- **渠道反馈**：异味、直播间集中退货等自动开质量事件。
- **精准召回**：
  - 可按成品批次、**按原料批次沿谱系扩散**、或由质量事件圈定范围；
  - 发布后冻结在库箱（含返工回库箱）、锁定已发货单与已售订单（到箱）；
  - 召回详情给出每个成品批次的**在库箱 / 已发货渠道 / 已售订单（含箱号）/ 留样检测结果 / 上游原料 / 退货情况**；
  - 自动/手工生成**停售通知、退货通知、赔偿方案、供应商追责函、监管备案报告、批次追溯报告**。
- **退货回厂三态处置（防库存失真）**：
  - `RETEST` 可复检 → 复检 PASS 回成品冷库、FAIL 强制销毁；
  - `REWORK` 可返工 → 返工完成回库（REWORKED，带返工标记）；
  - `DESTROY` 必须销毁，进入销毁区；
  - 处置汇总含库存状态一致性核对。
- **财务追偿**：供应商索赔与扣款（可拉黑/置不合格）、客服赔付、召回成本汇总（赔付+销毁成本−扣款回收=净损失）。
- **谱系追溯与看板**：原料正向（进入哪些成品、发往哪些渠道、留样是否合格）、成品反向全链路、管理者看板。
- **工程化**：JWT 风格 Bearer 会话（存 Redis，7 种角色 RBAC）、单据按前缀+日期取号（Redis 锁防并发重号）、
  OpenAPI 3.x 文档 + 自带 Swagger UI（静态资源内置，离线可用）、健康检查、一键 Docker Compose。

## 二、技术栈与结构

- Go 1.22（net/http ServeMux 方法路由）、PostgreSQL 13+（开发/镜像使用 15）、Redis 7。
- 仅少量依赖：`lib/pq`、`redis/go-redis/v9`、`google/uuid`、`golang.org/x/crypto/bcrypt`。

```
.
├── main.go / app.go            # 入口、配置、路由、鉴权、迁移
├── handler_*.go                # 各业务域 HTTP 处理器
├── migrations/001_init.sql     # 34 张业务表
├── openapi.yaml                # OpenAPI 3.x（51 路径 / 71 操作）
├── web/                        # Swagger UI（静态资源内置）
├── integration_test.go         # 端到端集成测试（覆盖需求全部主场景）
├── rbac_test.go                # 角色权限测试
├── scripts/run-tests.sh        # 一键测试（docker compose 或本机 PG/Redis）
├── scripts/smoke.sh            # 对运行中服务的 curl 端到端冒烟
├── Dockerfile / docker-compose.yml
```

## 三、一键启动（Docker Compose，推荐）

需要 Docker 与 Compose 插件。

```bash
docker compose up -d --build
# 等待约 20 秒后
curl -s http://localhost:8080/healthz
# {"service":"seafood-trace","status":"ok"}
```

- API 服务： http://localhost:8080
- Swagger UI： http://localhost:8080/swagger/
- OpenAPI： http://localhost:8080/openapi.yaml
- PostgreSQL： localhost:5432（seafood/seafood/seafood）
- Redis： localhost:6379

首次启动会自动执行数据库迁移并写入种子数据（测试账号、2 家供应商、4 个渠道、3 条产线、
7 个库位、3 个调味料批次、5 个包装规格）。停止并保留数据用 `docker compose stop`；清空重来用
`docker compose down -v`。

## 四、本地开发启动（已有 PostgreSQL / Redis）

需要 Go 1.22+。数据库由服务启动时自动迁移，只需先建空库。

```bash
# 1) 准备 PostgreSQL（任一可达实例）与 Redis
createdb seafood            # 例如 psql/createdb

# 2) 配置并启动（二选一）
export DATABASE_URL="host=localhost port=5432 user=postgres password=postgres dbname=seafood sslmode=disable"
export REDIS_ADDR=localhost:6379
go run .
#   或使用离散变量 PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE/PGSSLMODE 与 REDIS_ADDR
```

环境变量：`PORT`（默认 8080）、`DATABASE_URL`（或 `PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE/PGSSLMODE`）、
`REDIS_ADDR`（默认 localhost:6379）、`REDIS_PASSWORD`、`REDIS_DB`（默认 0）。

## 五、测试账号

| 用户名 | 密码 | 角色 | 主要权限 |
|---|---|---|---|
| admin | admin123 | admin | 全部主数据、入库、生产、品控、召回、财务 |
| qa | qa123 | qa | 温控/品控确认、留样、监管抽样、发起与发布召回、退货分级 |
| prod | prod123 | production | 计划、解冻、工序、完工 |
| wh | wh123 | warehouse | 入库、库存、盘点、退货回厂 |
| sales | sales123 | sales | 发货、订单、渠道反馈、包装规格 |
| finance | finance123 | finance | 供应商索赔扣款、客服赔付、召回成本 |
| regulator | reg123 | regulator | 监管视角只读 + 监管抽样登记 |

登录获取令牌：

```bash
TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
curl -s http://localhost:8080/api/v1/dashboard -H "Authorization: Bearer $TOKEN"
```

## 六、验证方式

### 方式 A：自动化端到端集成测试（已实际执行通过）

`integration_test.go` 用 httptest + 真实 PostgreSQL/Redis 跑通完整业务闭环并断言，覆盖：

1. 虾仁合格入库、扇贝 **-15℃ 入库超标**自动冻结并开事件；仓储巡检再次超标；
2. 解冻前对 ON_HOLD 原料 **409 拦截**，品控驳回后解冻；
3. **L1/L2 两条产线并行**；L1 解冻 300 分钟**解冻超时**；
4. 工序中**调味料用错、金检报警、包装漏气**均自动开质量事件；
5. 完工生成成品/箱号/谱系/留样，留样 PASS；
6. 渠道定制包装发货、已售订单（到箱）、**渠道异味反馈**、**直播间集中退货**；
7. **监管抽样不合格** → 以事件发起 CLASS_I 召回，范围精准锁定单一批次；
8. 发布召回：冻结在库 1 箱、锁定 1 发货单与 2 个订单，自动生成材料；
9. 召回详情精准到渠道/订单/箱/留样结论；冻结批次再发货被拦截；
10. 退货三态：复检 PASS 回库、销毁、返工完成回库；处置后库存状态一致性核对通过；
11. 供应商索赔扣款并拉黑、客服赔付支付、召回成本汇总；
12. 原料正向追溯到 2 个成品批次、成品反向追溯到原料与反馈；
13. 按**原料批次二次召回**扩散到 2 个成品批次，复检/返工回库箱被再次冻结；
14. 冷库盘点、看板统计；以及 RBAC（无令牌 401、伪造令牌 401、越权 403）。

运行：

```bash
./scripts/run-tests.sh
# 或显式指定：
TEST_PG_DSN="host=127.0.0.1 port=5432 user=seafood password=seafood dbname=seafood_test sslmode=disable" \
TEST_REDIS_ADDR=127.0.0.1:6379 go test -v .
```

> 脚本会自动选择 docker compose 或本机/用户态 PostgreSQL：若两者都没有，需先按第三节启动 compose。

本次交付在本开发机已实际执行（用户态 PostgreSQL 15 端口 5433 + 本机 Redis）：

```
=== RUN   TestFullLifecycle
--- PASS: TestFullLifecycle (1.18s)
=== RUN   TestRBAC
--- PASS: TestRBAC (0.29s)
PASS
ok  	seafood-trace	1.477s
```

> 说明：交付环境未安装 Docker 守护进程，因此 `docker compose up --build` 未在本机实跑；
> Dockerfile 多阶段构建（Go 1.22 静态编译）与 compose 三服务编排、健康检查均已配置，
> 等价的构建步骤（`go mod download` → `go build`）与对真实 PostgreSQL/Redis 的运行、
> 迁移、全部接口冒烟已在本机用同一 Go 工具链与官方 postgres:15/redis:7 兼容的环境实际验证通过。

### 方式 B：对运行中的服务手动冒烟

```bash
docker compose up -d --build      # 或本地 go run .
./scripts/smoke.sh                # 默认 BASE=http://localhost:8080
```

脚本会依次完成登录、入库、解冻超时、调味料用错、完工、发货、异味反馈、按原料召回发布、
退货复检、供应商扣款、客户赔付、成本与谱系查询。

### 方式 C：Swagger UI 手工点选

打开 http://localhost:8080/swagger/ ，先调 `POST /api/v1/auth/login`，点右上角 **Authorize**
填入返回的 token，即可在页面上调用全部 71 个接口。

## 七、关键业务规则（可调）

存于 `system_config`，可通过 `GET/PUT /api/v1/config` 调整：

| 键 | 默认值 | 含义 |
|---|---|---|
| inbound_temp_limit_c | -18 | 冻品入库温度上限（℃） |
| storage_temp_limit_c | -18 | 冷库储存温度上限（℃） |
| thaw_max_minutes | 240 | 解冻最长允许时长（分钟） |
| thaw_center_temp_limit_c | 7 | 解冻结束中心温度上限（℃） |
| destroy_unit_cost_cny | 20 | 每箱销毁处置成本（元），用于召回成本汇总 |

## 八、单据号规则

`前缀-yyyyMMdd-4位流水`（PostgreSQL 按前缀+日取号，Redis 分布式锁防多产线并发重号）：
RM 原料、PP 计划、PB 成品、SH 发货、SO 订单、QE 质量事件、GS 监管抽样、RS 留样、
RC 召回、DN 召回材料、ST 盘点、CL 供应商索赔、CP 赔付。
