# 海鲜预制菜工厂 · 批次解冻加工与召回服务

面向虾仁 / 扇贝 / 鱿鱼 / 鱼柳冻品的**采购入库 → 解冻加工 → 成品出库 → 渠道销售 → 异常召回**全链路批次追溯与召回系统。
后端 Go（标准库 net/http + Go 1.22 路由）、PostgreSQL 15、Redis 7，纯 API 服务，自带 **OpenAPI 3 文档与 Swagger UI**，提供 **Dockerfile / docker-compose 一键启动**。

---

## 1. 它解决什么问题

工厂接到冻品后，系统把 **采购入库、仓储温控、生产记录、品控结论、渠道反馈、客服赔付、财务追偿** 全部关联到**原料批次**与**成品批次**：

- **采购入库**：供应商、捕捞/生产日期、入库温度、检疫证明、逐箱箱号与库位；入库超温自动隔离并登记品控事件。
- **解冻加工（多条生产线并行）**：解冻开始/结束时间、中心温度、用水批次、操作人；清洗、切配、调味、称重、包装、金检、留样、入成品冷库逐工序记录。
- **自动质量拦截**：解冻超时、解冻中心温度超标、调味料批次用错、金检报警自动生成品控事件，严重问题**冻结工单**，品控结论后才可放行。
- **渠道定制包装**：按 渠道 × 成品 配置专属标签，包装工序自动套用，未配置拒绝包装。
- **召回**：按原料批次或品控事件发起，自动扩散到所有相关成品批次（含返工产出批），**定位已发货渠道、在库/冻结库存、已售订单、留样检测结果、原料箱**，一键发布并生成**停售、退货、赔偿、供应商追责、监管报告**全套材料。
- **退货回厂三态处置**：`REINSPECT` 可复检（合格回可售 / 不合格销毁）、`REWORK` 可返工（产出新批次并重新留样，保留 `rework_of` 溯源链）、`DESTROY` 必须销毁。库存按**可售 / 召回冻结 / 隔离待处置 / 已发 / 销毁 / 返工**六个分量记账，召回后库存不失真。
- **财务追偿**：客服赔付/退款单、供应商扣款追偿单全流程状态管理。
- **冷库盘点**：冻结账面快照、逐批录入实盘、盘差只调可售分量（不冲销召回冻结量）。
- **双向溯源**：原料批次正向（→ 成品 → 渠道/订单/留样/召回）、成品批次反向（→ 原料/供应商/箱/工序/金检/留样），结果经 Redis 缓存。

### 并发与一致性

- 工单级 **Redis 分布式锁（SET NX PX + Lua 释放）+ PostgreSQL 行锁（SELECT … FOR UPDATE）+ 事务**，保障多产线并行上报时同一工单串行更新。
- 写接口支持 **`Idempotency-Key`** 请求头：同键 24h 内重放返回首次结果（Redis 快速路径 + PG 持久兜底）。
- 所有库存变化写 **inventory_movements 台账**，可审计、可核对。

---

## 2. 一键启动（Docker Compose，推荐）

需要本机 Docker（含 Compose 插件）。

```bash
docker compose up --build
```

启动后：

| 服务 | 地址 |
|---|---|
| API 基址 | http://localhost:8080 |
| Swagger UI（在线调试） | http://localhost:8080/swagger/ |
| OpenAPI 3 文档 | http://localhost:8080/openapi.yaml |
| 健康检查 | http://localhost:8080/healthz |
| PostgreSQL | localhost:5432（seafood/seafood/seafood） |
| Redis | localhost:6379 |

应用启动时自动执行 `migrations/*.sql`（含测试账号、原料/产线/库位字典与温控阈值种子数据）。

停止并清理数据卷：

```bash
docker compose down -v
```

---

## 3. 本地开发方式（不用 Docker）

前置：Go ≥ 1.22、PostgreSQL ≥ 13、Redis ≥ 6。

```bash
# 1) 建库（以本机默认 postgres 超级用户为例）
createdb seafood

# 2) 配置依赖地址并启动（应用会自动跑迁移）
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/seafood?sslmode=disable"
export REDIS_ADDR="localhost:6379"
export HTTP_ADDR=":8080"
export MIGRATIONS_DIR="./migrations"
go run .
```

数据库 Schema 也可手工执行：`psql "$DATABASE_URL" -f migrations/001_schema.sql -f migrations/002_seed.sql`。

温控阈值在 `settings` 表可调（种子默认值）：

| key | 默认 | 含义 |
|---|---|---|
| raw_inbound_temp_max_c | -15 | 冻品入库温度上限（℃），超限自动隔离 |
| storage_temp_max_c | -18 | 仓储温度告警上限（℃） |
| thaw_max_duration_min | 720 | 解冻最长时长（分钟），超时生成事件 |
| thaw_center_temp_max_c | 10 | 解冻中心温度上限（℃），超限冻结工单 |
| recall_auto_level | II | 未指定级别时的召回级别 |

---

## 4. 测试账号（Bearer Token）

所有 `/api/v1/**` 接口都要在请求头带 `Authorization: Bearer <token>`（Swagger UI 已预填 manager token）：

| 角色 | 用户名 | Token | 主要权限 |
|---|---|---|---|
| 工厂管理者 | manager | `token-manager-001` | 主数据、计划工单、发货、召回、盘点 |
| 品控主管 | qc | `token-qc-002` | 入库、成品产出、留样/监管结论、品控结论、召回与退货处置、盘点 |
| 车间操作人 | op | `token-op-003` | 解冻、投料、工序、金检、温控、渠道反馈、退货登记 |
| 财务 | finance | `token-finance-004` | 赔付、供应商扣款 |

---

## 5. 验证方式（我实际执行过的验证）

### 5.1 自动化端到端冒烟（49 项断言全部通过）

服务启动后执行：

```bash
bash scripts/smoke.sh
# 或指定地址：BASE_URL=http://localhost:8080 bash scripts/smoke.sh
```

脚本依赖 `curl` 与 `jq`，按业务时序完整跑通 16 个章节，结尾输出：

```
==================== 结果 ====================
PASS=49  FAIL=0
```

覆盖的关键场景：

1. 健康检查（PG + Redis 在线）
2. 供应商 / 成品 BOM / 调味料 / 渠道 / 渠道定制包装
3. 采购入库：合格原料在库；**入库温度 -12℃ 超 -15℃ 上限 → 批次/箱自动 quarantined + RAW_TEMP_OVER 事件**
4. 同一计划在 **L1、L2 两条产线并行**下单；**隔离原料禁止解冻**
5. L1 全工序：解冻（开始/中心温度/用水批次/操作人）→ 投料（精确到箱）→ 清洗/切配/调味（正确腌料）/称重/包装（套用直播间定制标签）→ 金检 PASS → 成品入冷库 + 自动留样
6. L2 异常链：**解冻 840 分钟 > 720 上限 → THAW_TIMEOUT**；**误用孜然腌料做虾 → SEASONING_WRONG 工单冻结**；冻结期间工序被拒；品控“问题不成立”结论后工单自动解冻恢复，随后产出成品
7. 留样放行：未放行成品禁止发货；两批次留样 qualified 后放行
8. 发货到直播间/商超（多批次拼车、扣可售库存）+ 3 笔已售订单
9. **渠道反馈异味 → 自动 CHANNEL_ODOR 事件**；**监管抽检不合格 → 自动 REGULATOR_RECALL 事件**
10. **按原料批次发起一级召回**：范围自动扩散到 2 个成品批次，定位 3 笔订单、原料箱 B01/B02、留样结论
11. **发布召回**：在库库存全冻结（on_hand=0、hold>0）；生成 STOP_SALE / CHANNEL_NOTICE / RETURN_REQUEST / COMPENSATION / SUPPLIER_CLAIM / REGULATOR_REPORT 六类材料；订单全部通知；自动按渠道汇总退款赔付单；原料批次与箱精准封存
12. **退货三态**：回厂数量不得超已发量；20kg 复检合格回可售；5kg 销毁（destroyed 分量）；5kg 返工产出**新成品批次 + 重新留样**（保留 rework_of 链）
13. 赔付支付回写订单状态；**供应商 50000 元扣款追偿结算**
14. 冷库盘点：账面快照、实盘录入、**+1kg 盘盈**只调可售分量
15. **双向溯源**：原料→成品（含返工新批，共 3 个）→ 2 个渠道 → 3 笔订单 → 召回单；成品→原料/供应商/箱（2 箱）→ ≥5 道工序；二次查询命中 Redis 缓存（`cached:true`）
16. **幂等**：同 `Idempotency-Key` 重复建供应商返回同一 id；**权限**：op 建供应商 403、无 Token 401

### 5.2 并发安全专项（已实际执行）

对同一工单**并发 8 个**“成品产出”请求：仅 1 个返回 201，其余返回 `409 工单正在被其他生产线/工序更新，请稍后重试`，该工单最终只挂 1 个成品批次——证明分布式锁 + 行锁有效防止重复产出。

### 5.3 静态检查与文档

- `go vet ./...` 无告警；`go build` 零错误。
- `web/swagger/openapi.yaml` 通过 YAML 解析校验，覆盖 **47 条路径**；Swagger UI 的页面、CSS、JS、YAML 均返回 200。

> 以上均为在交付环境实际执行并确认的结果；Docker 镜像构建本身依赖宿主 Docker 守护进程，请在装有 Docker 的机器上执行第 2 节命令复验。

---

## 6. 典型调用速览

```bash
# 入库（超温会自动隔离并产生 temperature_alert）
curl -sS -X POST localhost:8080/api/v1/inbound/raw-batches \
  -H "Authorization: Bearer token-qc-002" -H 'Content-Type: application/json' \
  -d '{"material_code":"M-SHRIMP","supplier_code":"SUP-A",
       "catch_or_production_date":"2026-09-20","inbound_temp_c":-18,
       "quarantine_cert_no":"QY-001",
       "boxes":[{"box_no":"B01","location_code":"FZ-A","weight_kg":100}]}'

# 按原料批次发起召回（返回渠道/库存/订单/留样/箱全景快照）
curl -sS -X POST localhost:8080/api/v1/recalls \
  -H "Authorization: Bearer token-qc-002" -H 'Content-Type: application/json' \
  -d '{"raw_batch_no":"RAW2026...","level":"I","reason":"挥发性盐基氮超标"}'

# 发布召回：冻结库存、通知订单、生成六类材料
curl -sS -X POST localhost:8080/api/v1/recalls/1/release \
  -H "Authorization: Bearer token-qc-002"

# 退货回厂 → 三态处置
curl -sS -X POST localhost:8080/api/v1/returns/1/disposition \
  -H "Authorization: Bearer token-qc-002" -H 'Content-Type: application/json' \
  -d '{"disposition":"REWORK","note":"仅外包装破损"}'
```

---

## 7. 工程结构

```
.
├── Dockerfile                 # 多阶段构建（golang:1.22-alpine → alpine 运行）
├── docker-compose.yml         # db + redis + app 一键编排（含健康检查）
├── main.go                    # 启动、迁移、鉴权/幂等/恢复中间件
├── routes.go                  # 路由注册 + Swagger 静态资源
├── cache.go                   # Redis：溯源缓存、工单分布式锁
├── handler_*.go               # 主数据/入库/生产/成品质量/销售/品控/召回/退货/财务/库存/溯源
├── migrations/
│   ├── 001_schema.sql         # 全部表结构
│   └── 002_seed.sql           # 测试账号、字典、温控阈值
├── web/swagger/               # OpenAPI3 + 本地内置 Swagger UI（embed 进二进制）
├── scripts/smoke.sh           # 端到端冒烟脚本（49 项断言）
└── README.md
```

### 核心数据模型

- `raw_batches / raw_boxes`：原料批次与逐箱记录（箱号、库位、状态）。
- `production_plans / work_orders / thaw_records / material_usages / process_events / metal_detector_checks`：计划、多产线工单、解冻、投料、五工序、金检。
- `finished_batches / finished_stock / inventory_movements / sample_retentions`：成品批次、六分量库存、台账、留样。
- `shipments / shipment_items / customer_orders / channel_feedback`：发货、订单、渠道反馈。
- `quality_events / regulatory_samples`：品控事件与监管抽样。
- `recalls / recall_scopes / recall_actions`：召回单、扩散范围、六类材料。
- `return_goods`：退货三态处置；`rework_of_id` 串起返工批次链。
- `compensation_records / supplier_deductions`：赔付与供应商扣款追偿。
- `stocktakes / stocktake_items`：冷库盘点。
