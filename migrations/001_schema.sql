-- 海鲜预制菜工厂：批次解冻加工与召回服务 schema
-- PostgreSQL 13+

CREATE TABLE IF NOT EXISTS users (
    id          BIGSERIAL PRIMARY KEY,
    username    VARCHAR(64) UNIQUE NOT NULL,
    display_name VARCHAR(64) NOT NULL,
    role        VARCHAR(32) NOT NULL CHECK (role IN ('manager','qc','operator','finance')),
    api_token   VARCHAR(128) UNIQUE NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS suppliers (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(128) NOT NULL,
    contact     VARCHAR(128),
    phone       VARCHAR(32),
    qualification_expires_at DATE,
    status      VARCHAR(16) NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 原料目录（虾仁 / 扇贝 / 鱿鱼 / 鱼柳 等冻品）
CREATE TABLE IF NOT EXISTS materials (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(64) NOT NULL,
    category    VARCHAR(32) NOT NULL CHECK (category IN ('SHRIMP','SCALLOP','SQUID','FISH_FILLET','OTHER')),
    unit        VARCHAR(8) NOT NULL DEFAULT 'kg',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 成品 SKU
CREATE TABLE IF NOT EXISTS products (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(128) NOT NULL,
    default_pack_spec VARCHAR(64),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS product_bom (
    product_id   BIGINT NOT NULL REFERENCES products(id),
    material_id  BIGINT NOT NULL REFERENCES materials(id),
    qty_per_unit NUMERIC(10,3) NOT NULL CHECK (qty_per_unit > 0),
    PRIMARY KEY (product_id, material_id)
);

-- 调味料目录（用于核对调味批次是否用错）
CREATE TABLE IF NOT EXISTS seasonings (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(64) NOT NULL
);
CREATE TABLE IF NOT EXISTS product_seasonings (
    product_id   BIGINT NOT NULL REFERENCES products(id),
    seasoning_id BIGINT NOT NULL REFERENCES seasonings(id),
    PRIMARY KEY (product_id, seasoning_id)
);

CREATE TABLE IF NOT EXISTS production_lines (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(64) NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'active' CHECK (status IN ('active','stopped')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 冷库 / 解冻间 / 成品库库位
CREATE TABLE IF NOT EXISTS locations (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(64) NOT NULL,
    zone        VARCHAR(16) NOT NULL CHECK (zone IN ('FREEZER','THAW_ROOM','FINISHED_COLD')),
    target_temp NUMERIC(5,2)
);

CREATE TABLE IF NOT EXISTS channels (
    id          BIGSERIAL PRIMARY KEY,
    code        VARCHAR(32) UNIQUE NOT NULL,
    name        VARCHAR(128) NOT NULL,
    type        VARCHAR(32) NOT NULL CHECK (type IN ('LIVE_ROOM','ECOMMERCE','SUPERMARKET','DISTRIBUTOR','OTHER')),
    contact     VARCHAR(128),
    status      VARCHAR(16) NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 渠道定制包装
CREATE TABLE IF NOT EXISTS channel_packagings (
    id          BIGSERIAL PRIMARY KEY,
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    product_id  BIGINT NOT NULL REFERENCES products(id),
    label_spec  VARCHAR(256) NOT NULL,
    UNIQUE (channel_id, product_id)
);

-- ============ 采购入库 ============
CREATE TABLE IF NOT EXISTS raw_batches (
    id              BIGSERIAL PRIMARY KEY,
    batch_no        VARCHAR(40) UNIQUE NOT NULL,
    material_id     BIGINT NOT NULL REFERENCES materials(id),
    supplier_id     BIGINT NOT NULL REFERENCES suppliers(id),
    catch_or_production_date DATE NOT NULL,   -- 捕捞或生产日期
    arrival_at      TIMESTAMPTZ NOT NULL,
    inbound_temp_c  NUMERIC(5,2) NOT NULL,    -- 入库温度
    quarantine_cert_no VARCHAR(64) NOT NULL, -- 检疫证明
    total_weight_kg NUMERIC(10,3) NOT NULL DEFAULT 0,
    status          VARCHAR(24) NOT NULL DEFAULT 'in_storage'
                    CHECK (status IN ('in_storage','quarantined','in_production','consumed','recalled','destroyed')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS raw_boxes (
    id            BIGSERIAL PRIMARY KEY,
    raw_batch_id  BIGINT NOT NULL REFERENCES raw_batches(id),
    box_no        VARCHAR(40) NOT NULL,       -- 箱号（可精准召回定位）
    location_id   BIGINT REFERENCES locations(id), -- 库位
    weight_kg     NUMERIC(10,3) NOT NULL,
    status        VARCHAR(24) NOT NULL DEFAULT 'in_storage'
                  CHECK (status IN ('in_storage','quarantined','used','recalled','destroyed','sampled')),
    UNIQUE (raw_batch_id, box_no)
);
CREATE INDEX IF NOT EXISTS idx_raw_boxes_batch ON raw_boxes(raw_batch_id);

-- 仓储温控记录
CREATE TABLE IF NOT EXISTS storage_temp_logs (
    id          BIGSERIAL PRIMARY KEY,
    raw_batch_id BIGINT REFERENCES raw_batches(id),
    location_id BIGINT REFERENCES locations(id),
    temp_c      NUMERIC(5,2) NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    recorder    VARCHAR(64)
);
CREATE INDEX IF NOT EXISTS idx_storage_temp_batch ON storage_temp_logs(raw_batch_id);

-- ============ 生产 ============
CREATE TABLE IF NOT EXISTS production_plans (
    id          BIGSERIAL PRIMARY KEY,
    plan_no     VARCHAR(40) UNIQUE NOT NULL,
    product_id  BIGINT NOT NULL REFERENCES products(id),
    planned_qty NUMERIC(10,3) NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'issued'
                CHECK (status IN ('issued','in_progress','done','cancelled')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS work_orders (
    id           BIGSERIAL PRIMARY KEY,
    order_no     VARCHAR(40) UNIQUE NOT NULL,
    plan_id      BIGINT NOT NULL REFERENCES production_plans(id),
    line_id      BIGINT NOT NULL REFERENCES production_lines(id),
    product_id   BIGINT NOT NULL REFERENCES products(id),
    planned_qty  NUMERIC(10,3) NOT NULL,
    status       VARCHAR(24) NOT NULL DEFAULT 'issued'
                 CHECK (status IN ('issued','thawing','in_production','on_hold','finished','cancelled')),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_wo_line_status ON work_orders(line_id, status);

CREATE TABLE IF NOT EXISTS thaw_records (
    id              BIGSERIAL PRIMARY KEY,
    work_order_id   BIGINT NOT NULL REFERENCES work_orders(id),
    raw_batch_id    BIGINT NOT NULL REFERENCES raw_batches(id),
    start_at        TIMESTAMPTZ NOT NULL,       -- 解冻开始时间
    end_at          TIMESTAMPTZ,
    start_center_temp_c NUMERIC(5,2),
    end_center_temp_c   NUMERIC(5,2),
    max_center_temp_c   NUMERIC(5,2),
    water_batch_no  VARCHAR(64) NOT NULL,        -- 用水批次
    operator        VARCHAR(64) NOT NULL,        -- 操作人
    duration_minutes INT,
    timed_out       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_thaw_wo ON thaw_records(work_order_id);

CREATE TABLE IF NOT EXISTS thaw_temp_logs (
    id            BIGSERIAL PRIMARY KEY,
    thaw_id       BIGINT NOT NULL REFERENCES thaw_records(id),
    center_temp_c NUMERIC(5,2) NOT NULL,
    read_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 原料投料（原料批次/箱 → 工单/成品批次 的核心关联）
CREATE TABLE IF NOT EXISTS material_usages (
    id            BIGSERIAL PRIMARY KEY,
    work_order_id BIGINT NOT NULL REFERENCES work_orders(id),
    raw_batch_id  BIGINT NOT NULL REFERENCES raw_batches(id),
    raw_box_id    BIGINT REFERENCES raw_boxes(id),
    qty_kg        NUMERIC(10,3) NOT NULL CHECK (qty_kg > 0),
    used_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    operator      VARCHAR(64)
);
CREATE INDEX IF NOT EXISTS idx_mu_raw ON material_usages(raw_batch_id);
CREATE INDEX IF NOT EXISTS idx_mu_wo ON material_usages(work_order_id);

-- 工序记录：清洗 / 切配 / 调味 / 称重 / 包装
CREATE TABLE IF NOT EXISTS process_events (
    id            BIGSERIAL PRIMARY KEY,
    work_order_id BIGINT NOT NULL REFERENCES work_orders(id),
    stage         VARCHAR(16) NOT NULL CHECK (stage IN ('WASHING','CUTTING','SEASONING','WEIGHING','PACKAGING')),
    operator      VARCHAR(64) NOT NULL,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    detail        JSONB NOT NULL DEFAULT '{}'::jsonb  -- 调味批次号/称重/定制包装等
);
CREATE INDEX IF NOT EXISTS idx_pe_wo ON process_events(work_order_id);

-- 金检
CREATE TABLE IF NOT EXISTS metal_detector_checks (
    id            BIGSERIAL PRIMARY KEY,
    work_order_id BIGINT NOT NULL REFERENCES work_orders(id),
    result        VARCHAR(16) NOT NULL CHECK (result IN ('PASS','ALARM')),
    operator      VARCHAR(64) NOT NULL,
    checked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    note          TEXT
);
CREATE INDEX IF NOT EXISTS idx_md_wo ON metal_detector_checks(work_order_id);

-- 成品批次
CREATE TABLE IF NOT EXISTS finished_batches (
    id              BIGSERIAL PRIMARY KEY,
    batch_no        VARCHAR(40) UNIQUE NOT NULL,
    product_id      BIGINT NOT NULL REFERENCES products(id),
    work_order_id   BIGINT NOT NULL REFERENCES work_orders(id),
    qty_kg          NUMERIC(10,3) NOT NULL,
    pack_spec       VARCHAR(64),                 -- 实际包装规格
    pack_channel_id BIGINT REFERENCES channels(id), -- 渠道定制包装
    status          VARCHAR(24) NOT NULL DEFAULT 'produced'
                    CHECK (status IN ('produced','released','shipped','recalled','returned','reworked','destroyed')),
    rework_of_id    BIGINT REFERENCES finished_batches(id),
    produced_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_fg_wo ON finished_batches(work_order_id);

-- 成品库存（按状态分量，避免召回/退货后库存失真）
CREATE TABLE IF NOT EXISTS finished_stock (
    finished_batch_id BIGINT PRIMARY KEY REFERENCES finished_batches(id),
    location_id       BIGINT REFERENCES locations(id),
    qty_on_hand       NUMERIC(10,3) NOT NULL DEFAULT 0, -- 可售库存
    qty_hold          NUMERIC(10,3) NOT NULL DEFAULT 0, -- 召回停售冻结
    qty_quarantined   NUMERIC(10,3) NOT NULL DEFAULT 0, -- 回厂待处置/待复检
    qty_shipped       NUMERIC(10,3) NOT NULL DEFAULT 0,
    qty_destroyed     NUMERIC(10,3) NOT NULL DEFAULT 0,
    qty_reworked      NUMERIC(10,3) NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS inventory_movements (
    id            BIGSERIAL PRIMARY KEY,
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    change_qty    NUMERIC(10,3) NOT NULL, -- 正入负出（作用于对应状态分量见 reason）
    balance_after NUMERIC(10,3),
    reason        VARCHAR(32) NOT NULL CHECK (reason IN
                    ('PRODUCTION_IN','SHIP_OUT','RETURN_QUARANTINE_IN',
                     'RECALL_HOLD','RECALL_RELEASE_HOLD',
                     'REINSPECT_PASS_IN','REINSPECT_FAIL_DESTROY',
                     'RETURN_REWORK_OUT','RETURN_DESTROY_OUT',
                     'REWORK_OUTPUT_IN','STOCKTAKE_ADJ')),
    ref_type      VARCHAR(32),
    ref_no        VARCHAR(40),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_im_fg ON inventory_movements(finished_batch_id);

-- 留样
CREATE TABLE IF NOT EXISTS sample_retentions (
    id            BIGSERIAL PRIMARY KEY,
    sample_no     VARCHAR(40) UNIQUE NOT NULL,
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    result        VARCHAR(16) NOT NULL DEFAULT 'pending'
                  CHECK (result IN ('pending','qualified','unqualified')),
    report_no     VARCHAR(64),
    lab           VARCHAR(128),
    conclusion    TEXT,
    sampled_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resulted_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_sr_fg ON sample_retentions(finished_batch_id);

-- ============ 销售 ============
CREATE TABLE IF NOT EXISTS shipments (
    id          BIGSERIAL PRIMARY KEY,
    shipment_no VARCHAR(40) UNIQUE NOT NULL,
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    status      VARCHAR(16) NOT NULL DEFAULT 'shipped' CHECK (status IN ('shipped','recalled','closed')),
    shipped_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    note        TEXT
);
CREATE TABLE IF NOT EXISTS shipment_items (
    id            BIGSERIAL PRIMARY KEY,
    shipment_id   BIGINT NOT NULL REFERENCES shipments(id),
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    qty_kg        NUMERIC(10,3) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_si_fg ON shipment_items(finished_batch_id);

CREATE TABLE IF NOT EXISTS customer_orders (
    id            BIGSERIAL PRIMARY KEY,
    order_no      VARCHAR(64) UNIQUE NOT NULL,
    channel_id    BIGINT NOT NULL REFERENCES channels(id),
    shipment_id   BIGINT REFERENCES shipments(id),
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    buyer         VARCHAR(128),
    qty_kg        NUMERIC(10,3) NOT NULL,
    amount        NUMERIC(12,2) NOT NULL DEFAULT 0,
    status        VARCHAR(24) NOT NULL DEFAULT 'sold'
                  CHECK (status IN ('sold','recall_notified','returned','refunded','compensated')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_co_fg ON customer_orders(finished_batch_id);

-- ============ 品控 / 异常 ============
CREATE TABLE IF NOT EXISTS quality_events (
    id            BIGSERIAL PRIMARY KEY,
    event_no      VARCHAR(40) UNIQUE NOT NULL,
    type          VARCHAR(32) NOT NULL CHECK (type IN
                    ('RAW_TEMP_OVER','THAW_TIMEOUT','THAW_TEMP_OVER','SEASONING_WRONG','METAL_ALARM',
                     'PACK_LEAK','SAMPLE_UNQUALIFIED','CHANNEL_ODOR','LIVE_RETURN','REGULATOR_RECALL','OTHER')),
    severity      VARCHAR(8) NOT NULL DEFAULT 'major' CHECK (severity IN ('minor','major','critical')),
    source        VARCHAR(32) NOT NULL DEFAULT 'manual',
    raw_batch_id    BIGINT REFERENCES raw_batches(id),
    finished_batch_id BIGINT REFERENCES finished_batches(id),
    work_order_id BIGINT REFERENCES work_orders(id),
    channel_id    BIGINT REFERENCES channels(id),
    description   TEXT NOT NULL,
    qc_conclusion TEXT,
    status        VARCHAR(16) NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','confirmed','recall_triggered','closed')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    VARCHAR(64)
);
CREATE INDEX IF NOT EXISTS idx_qe_raw ON quality_events(raw_batch_id);
CREATE INDEX IF NOT EXISTS idx_qe_fg ON quality_events(finished_batch_id);

-- 渠道反馈（异味 / 直播间集中退货等）
CREATE TABLE IF NOT EXISTS channel_feedback (
    id          BIGSERIAL PRIMARY KEY,
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    finished_batch_id BIGINT REFERENCES finished_batches(id),
    order_id    BIGINT REFERENCES customer_orders(id),
    feedback_type VARCHAR(24) NOT NULL CHECK (feedback_type IN
                    ('ODOR','CONCENTRATED_RETURN','QUALITY_COMPLAINT','OTHER')),
    content     TEXT NOT NULL,
    quality_event_id BIGINT REFERENCES quality_events(id),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 监管抽样
CREATE TABLE IF NOT EXISTS regulatory_samples (
    id          BIGSERIAL PRIMARY KEY,
    sample_no   VARCHAR(40) UNIQUE NOT NULL,
    regulator   VARCHAR(128) NOT NULL,
    raw_batch_id BIGINT REFERENCES raw_batches(id),
    finished_batch_id BIGINT REFERENCES finished_batches(id),
    result      VARCHAR(16) NOT NULL DEFAULT 'pending' CHECK (result IN ('pending','qualified','unqualified')),
    report_no   VARCHAR(64),
    conclusion  TEXT,
    sampled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resulted_at TIMESTAMPTZ
);

-- ============ 召回 ============
CREATE TABLE IF NOT EXISTS recalls (
    id            BIGSERIAL PRIMARY KEY,
    recall_no     VARCHAR(40) UNIQUE NOT NULL,
    trigger_event_id BIGINT REFERENCES quality_events(id),
    raw_batch_id  BIGINT REFERENCES raw_batches(id),
    level         VARCHAR(8) NOT NULL CHECK (level IN ('I','II','III')),
    reason        TEXT NOT NULL,
    status        VARCHAR(16) NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','released','executing','closed')),
    created_by    VARCHAR(64),
    released_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS recall_scopes (
    recall_id        BIGINT NOT NULL REFERENCES recalls(id),
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    PRIMARY KEY (recall_id, finished_batch_id)
);

-- 召回材料/动作：停售、退货、赔偿、供应商追责、监管报告
CREATE TABLE IF NOT EXISTS recall_actions (
    id          BIGSERIAL PRIMARY KEY,
    recall_id   BIGINT NOT NULL REFERENCES recalls(id),
    action_type VARCHAR(24) NOT NULL CHECK (action_type IN
                    ('STOP_SALE','RETURN_REQUEST','COMPENSATION','SUPPLIER_CLAIM','REGULATOR_REPORT','CHANNEL_NOTICE')),
    target_type VARCHAR(16) NOT NULL DEFAULT 'system' CHECK (target_type IN ('system','channel','supplier','regulator','order')),
    target_ref  VARCHAR(64),
    title       VARCHAR(200) NOT NULL,
    content     TEXT NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'drafted' CHECK (status IN ('drafted','issued','executed','void')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    executed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_ra_recall ON recall_actions(recall_id);

-- ============ 退货回厂与处置 ============
CREATE TABLE IF NOT EXISTS return_goods (
    id          BIGSERIAL PRIMARY KEY,
    return_no   VARCHAR(40) UNIQUE NOT NULL,
    recall_id   BIGINT REFERENCES recalls(id),
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    channel_id  BIGINT REFERENCES channels(id),
    order_id    BIGINT REFERENCES customer_orders(id),
    qty_kg      NUMERIC(10,3) NOT NULL,
    status      VARCHAR(24) NOT NULL DEFAULT 'arrived'
                CHECK (status IN ('arrived','reinspect_pending','reinspect_pass','reinspect_fail',
                                  'rework_pending','reworked','destroyed')),
    disposition VARCHAR(16) CHECK (disposition IN ('REINSPECT','REWORK','DESTROY')),
    reason      TEXT,
    arrived_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at  TIMESTAMPTZ,
    processed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_ret_fg ON return_goods(finished_batch_id);

-- ============ 财务：赔付 / 供应商扣款追偿 ============
CREATE TABLE IF NOT EXISTS compensation_records (
    id          BIGSERIAL PRIMARY KEY,
    comp_no     VARCHAR(40) UNIQUE NOT NULL,
    recall_id   BIGINT REFERENCES recalls(id),
    order_id    BIGINT REFERENCES customer_orders(id),
    channel_id  BIGINT REFERENCES channels(id),
    amount      NUMERIC(12,2) NOT NULL CHECK (amount >= 0),
    kind        VARCHAR(16) NOT NULL CHECK (kind IN ('REFUND','COMPENSATION')),
    status      VARCHAR(16) NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed','confirmed','paid')),
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS supplier_deductions (
    id           BIGSERIAL PRIMARY KEY,
    deduct_no    VARCHAR(40) UNIQUE NOT NULL,
    supplier_id  BIGINT NOT NULL REFERENCES suppliers(id),
    recall_id    BIGINT REFERENCES recalls(id),
    raw_batch_id BIGINT REFERENCES raw_batches(id),
    amount       NUMERIC(14,2) NOT NULL CHECK (amount >= 0),
    reason       TEXT NOT NULL,
    status       VARCHAR(16) NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed','confirmed','settled')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);

-- ============ 盘点 ============
CREATE TABLE IF NOT EXISTS stocktakes (
    id          BIGSERIAL PRIMARY KEY,
    take_no     VARCHAR(40) UNIQUE NOT NULL,
    location_id BIGINT REFERENCES locations(id),
    status      VARCHAR(16) NOT NULL DEFAULT 'counting' CHECK (status IN ('counting','confirmed','cancelled')),
    counted_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS stocktake_items (
    id          BIGSERIAL PRIMARY KEY,
    stocktake_id BIGINT NOT NULL REFERENCES stocktakes(id),
    finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
    system_qty  NUMERIC(10,3) NOT NULL,
    counted_qty NUMERIC(10,3),
    diff_qty    NUMERIC(10,3),
    UNIQUE (stocktake_id, finished_batch_id)
);

CREATE TABLE IF NOT EXISTS settings (
    key   VARCHAR(64) PRIMARY KEY,
    value VARCHAR(200) NOT NULL
);

-- 单号序列
CREATE SEQUENCE IF NOT EXISTS seq_doc START 1000;

-- 幂等键（持久层兜底，Redis 为主）
CREATE TABLE IF NOT EXISTS idempotency_keys (
    idem_key    VARCHAR(128) PRIMARY KEY,
    response    JSONB,
    status_code INT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
