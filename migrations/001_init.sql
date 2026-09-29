-- 海鲜预制菜工厂 批次解冻加工与召回服务 初始化 schema
-- PostgreSQL 13+

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin','qa','production','warehouse','sales','finance','regulator')),
    full_name     TEXT NOT NULL,
    active        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 业务单号序列（按前缀按天取号）
CREATE TABLE doc_sequences (
    prefix TEXT NOT NULL,
    day    TEXT NOT NULL,
    value  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (prefix, day)
);

-- 可调业务规则
CREATE TABLE system_config (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO system_config (key, value, description) VALUES
 ('inbound_temp_limit_c',  '-18', '冻品入库温度上限(℃)，高于即超标'),
 ('storage_temp_limit_c',  '-18', '冷库储存温度上限(℃)，高于即超标'),
 ('thaw_max_minutes',      '240', '解冻最长允许时长(分钟)，超过即解冻超时'),
 ('thaw_center_temp_limit_c','7',  '解冻结束中心温度上限(℃)'),
 ('destroy_unit_cost_cny', '20',  '每箱销毁处置成本(元)，用于财务追偿汇总')
ON CONFLICT DO NOTHING;

-- ============ 主数据 ============

CREATE TABLE suppliers (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code             TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    contact_person   TEXT,
    phone            TEXT,
    qualification_no TEXT,
    qualified        BOOLEAN NOT NULL DEFAULT TRUE,
    blacklisted      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE channels (
    id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code    TEXT NOT NULL UNIQUE,
    name    TEXT NOT NULL,
    type    TEXT NOT NULL CHECK (type IN ('RESTAURANT','RETAIL','ECOMMERCE','LIVESTREAM','DISTRIBUTOR')),
    contact_person TEXT,
    phone   TEXT,
    active  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE production_lines (
    id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code    TEXT NOT NULL UNIQUE,
    name    TEXT NOT NULL,
    workshop TEXT NOT NULL DEFAULT '加工车间',
    active  BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE TABLE storage_locations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,   -- 如 DKC-A-01-03
    name        TEXT NOT NULL,
    zone        TEXT NOT NULL CHECK (zone IN ('RAW_FROZEN','FINISHED_COLD','THAW_ROOM','QUARANTINE','DESTROY')),
    capacity_kg NUMERIC(12,2),
    active      BOOLEAN NOT NULL DEFAULT TRUE
);

-- 调味料批次
CREATE TABLE seasoning_batches (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_no        TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    supplier_id     BIGINT REFERENCES suppliers(id),
    production_date DATE,
    expiry_date     DATE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 渠道定制包装规格
CREATE TABLE packaging_specs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    channel_id  BIGINT REFERENCES channels(id),  -- null = 通用包装
    product_code TEXT,
    spec_detail TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============ 采购入库 / 仓储温控 ============

CREATE TABLE raw_material_batches (
    id                      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_no                TEXT NOT NULL UNIQUE,          -- 原料批次号
    supplier_id             BIGINT NOT NULL REFERENCES suppliers(id),
    material_type           TEXT NOT NULL CHECK (material_type IN ('SHRIMP','SCALLOP','SQUID','FISH_FILLET')),
    catch_or_production_date DATE NOT NULL,                -- 捕捞或生产日期
    arrival_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    inbound_temp_c          NUMERIC(5,2) NOT NULL,         -- 入库温度
    temp_limit_c            NUMERIC(5,2) NOT NULL DEFAULT -18,
    quarantine_cert_no      TEXT NOT NULL,                 -- 检疫证明
    inspector               TEXT NOT NULL,
    total_weight_kg         NUMERIC(12,2) NOT NULL DEFAULT 0,
    status                  TEXT NOT NULL DEFAULT 'FROZEN_IN'
                            CHECK (status IN ('FROZEN_IN','ON_HOLD','RELEASED','CONSUMED','CLOSED')),
    note                    TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE raw_boxes (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id    BIGINT NOT NULL REFERENCES raw_material_batches(id),
    box_no      TEXT NOT NULL,
    weight_kg   NUMERIC(10,2) NOT NULL,
    location_id BIGINT REFERENCES storage_locations(id),
    status      TEXT NOT NULL DEFAULT 'IN_STORAGE'
                CHECK (status IN ('IN_STORAGE','THAWING','CONSUMED','DESTROYED')),
    inbound_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (batch_id, box_no)
);
CREATE INDEX idx_raw_boxes_batch ON raw_boxes(batch_id);
CREATE INDEX idx_raw_boxes_location ON raw_boxes(location_id);

CREATE TABLE temp_logs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id    BIGINT REFERENCES raw_material_batches(id),
    box_id      BIGINT REFERENCES raw_boxes(id),
    phase       TEXT NOT NULL CHECK (phase IN ('INBOUND','STORAGE')),
    temp_c      NUMERIC(5,2) NOT NULL,
    limit_c     NUMERIC(5,2) NOT NULL,
    violation   BOOLEAN NOT NULL DEFAULT FALSE,
    recorded_by TEXT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    note        TEXT
);
CREATE INDEX idx_temp_logs_batch ON temp_logs(batch_id);

-- ============ 生产计划 / 解冻 / 加工工序 ============

CREATE TABLE production_plans (
    id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plan_no TEXT NOT NULL UNIQUE,
    line_id BIGINT NOT NULL REFERENCES production_lines(id),
    product_code TEXT NOT NULL,
    product_name TEXT NOT NULL,
    planned_qty_kg NUMERIC(12,2) NOT NULL,
    expected_seasoning_batch_id BIGINT REFERENCES seasoning_batches(id),
    status  TEXT NOT NULL DEFAULT 'ISSUED'
            CHECK (status IN ('ISSUED','IN_PROGRESS','DONE','CANCELLED')),
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    done_at    TIMESTAMPTZ
);

CREATE TABLE thaw_records (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plan_id        BIGINT NOT NULL REFERENCES production_plans(id),
    line_id        BIGINT NOT NULL REFERENCES production_lines(id),
    raw_batch_id   BIGINT NOT NULL REFERENCES raw_material_batches(id),
    water_batch_no TEXT NOT NULL,          -- 用水批次
    operator       TEXT NOT NULL,
    start_at       TIMESTAMPTZ NOT NULL,
    end_at         TIMESTAMPTZ,
    center_temp_c  NUMERIC(5,2),           -- 解冻中心温度
    duration_min   INTEGER,
    raw_weight_kg  NUMERIC(12,2) NOT NULL DEFAULT 0,
    timeout        BOOLEAN NOT NULL DEFAULT FALSE,
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_thaw_plan ON thaw_records(plan_id);
CREATE INDEX idx_thaw_rawbatch ON thaw_records(raw_batch_id);

-- 工序记录：清洗/切配/调味/称重/包装/金检/留样/入成品冷库
CREATE TABLE processing_records (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plan_id             BIGINT NOT NULL REFERENCES production_plans(id),
    line_id             BIGINT NOT NULL REFERENCES production_lines(id),
    stage               TEXT NOT NULL
                        CHECK (stage IN ('CLEAN','CUT','SEASON','WEIGH','PACK','METAL_CHECK','SAMPLE','COLD_IN')),
    seq                 INTEGER NOT NULL,
    operator            TEXT NOT NULL,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at            TIMESTAMPTZ,
    seasoning_batch_id  BIGINT REFERENCES seasoning_batches(id),
    weight_kg           NUMERIC(12,2),
    packaging_spec_id   BIGINT REFERENCES packaging_specs(id),
    package_leak        BOOLEAN,
    metal_alarm         BOOLEAN,
    sample_qty_g        INTEGER,
    result              TEXT NOT NULL DEFAULT 'OK' CHECK (result IN ('OK','NG')),
    note                TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_processing_plan ON processing_records(plan_id);

-- ============ 成品批次 / 谱系 / 留样 ============

CREATE TABLE product_batches (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_no       TEXT NOT NULL UNIQUE,
    plan_id        BIGINT NOT NULL REFERENCES production_plans(id),
    line_id        BIGINT NOT NULL REFERENCES production_lines(id),
    product_code   TEXT NOT NULL,
    product_name   TEXT NOT NULL,
    qty_kg         NUMERIC(12,2) NOT NULL,
    cold_location_id BIGINT REFERENCES storage_locations(id),
    status         TEXT NOT NULL DEFAULT 'COLD_STORAGE'
                   CHECK (status IN ('COLD_STORAGE','RELEASED','PARTIAL_SHIPPED','SHIPPED','RECALL_HELD','RECALLED','CLOSED')),
    produced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_product_plan ON product_batches(plan_id);

-- 原料批次 -> 成品批次 双向谱系
CREATE TABLE material_genealogy (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_batch_id BIGINT NOT NULL REFERENCES product_batches(id),
    raw_batch_id     BIGINT NOT NULL REFERENCES raw_material_batches(id),
    qty_consumed_kg  NUMERIC(12,2) NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product_batch_id, raw_batch_id)
);
CREATE INDEX idx_gene_raw ON material_genealogy(raw_batch_id);
CREATE INDEX idx_gene_product ON material_genealogy(product_batch_id);

CREATE TABLE product_boxes (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    batch_id      BIGINT NOT NULL REFERENCES product_batches(id),
    box_no        TEXT NOT NULL UNIQUE,
    weight_kg     NUMERIC(10,2) NOT NULL,
    location_id   BIGINT REFERENCES storage_locations(id),
    packaging_spec_id BIGINT REFERENCES packaging_specs(id), -- 实际贴标包装（渠道定制）
    status        TEXT NOT NULL DEFAULT 'IN_STOCK'
                  CHECK (status IN ('IN_STOCK','RECALL_HELD','RESERVED','SHIPPED','SOLD',
                                    'RETURN_HOLD','RETESTING','REWORKING','REWORKED','DESTROYED')),
    reworked      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_pbox_batch ON product_boxes(batch_id);
CREATE INDEX idx_pbox_location ON product_boxes(location_id);
CREATE INDEX idx_pbox_status ON product_boxes(status);

-- 留样与实验室检测
CREATE TABLE retained_samples (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    product_batch_id BIGINT NOT NULL REFERENCES product_batches(id),
    box_id           BIGINT REFERENCES product_boxes(id),
    sample_no        TEXT NOT NULL UNIQUE,
    qty_g            INTEGER NOT NULL,
    storage_location TEXT,
    retained_by      TEXT NOT NULL,
    retained_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    lab_result       TEXT NOT NULL DEFAULT 'PENDING' CHECK (lab_result IN ('PENDING','PASS','FAIL')),
    lab_report_no    TEXT,
    tested_at        TIMESTAMPTZ,
    conclusion       TEXT
);
CREATE INDEX idx_sample_batch ON retained_samples(product_batch_id);

-- ============ 发货 / 订单 / 渠道反馈 ============

CREATE TABLE shipments (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ship_no     TEXT NOT NULL UNIQUE,
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    product_batch_id BIGINT NOT NULL REFERENCES product_batches(id),
    packaging_spec_id BIGINT REFERENCES packaging_specs(id),
    vehicle_no  TEXT,
    operator    TEXT NOT NULL,
    total_boxes INTEGER NOT NULL DEFAULT 0,
    total_kg    NUMERIC(12,2) NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'SHIPPED'
                CHECK (status IN ('SHIPPED','PARTIAL_RETURN','FULL_RETURN','RECALLED')),
    shipped_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ship_batch ON shipments(product_batch_id);
CREATE INDEX idx_ship_channel ON shipments(channel_id);

CREATE TABLE shipment_boxes (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    shipment_id BIGINT NOT NULL REFERENCES shipments(id),
    box_id      BIGINT NOT NULL REFERENCES product_boxes(id) UNIQUE
);
CREATE INDEX idx_shipbox_ship ON shipment_boxes(shipment_id);

CREATE TABLE sales_orders (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_no    TEXT NOT NULL UNIQUE,
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    shipment_id BIGINT REFERENCES shipments(id),
    customer_ref TEXT,                    -- 脱敏客户标识
    amount_cny  NUMERIC(12,2) NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'SOLD'
                CHECK (status IN ('SOLD','RETURNED','RECALLED','COMPENSATED')),
    sold_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_order_channel ON sales_orders(channel_id);

CREATE TABLE order_items (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id         BIGINT NOT NULL REFERENCES sales_orders(id),
    product_batch_id BIGINT NOT NULL REFERENCES product_batches(id),
    box_id           BIGINT REFERENCES product_boxes(id),
    qty              INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_oi_order ON order_items(order_id);
CREATE INDEX idx_oi_batch ON order_items(product_batch_id);

CREATE TABLE channel_feedbacks (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    order_id    BIGINT REFERENCES sales_orders(id),
    product_batch_id BIGINT REFERENCES product_batches(id),
    box_no      TEXT,
    source      TEXT NOT NULL CHECK (source IN ('CHANNEL','LIVESTREAM','REGULATOR','CUSTOMER_SERVICE')),
    type        TEXT NOT NULL CHECK (type IN ('OFF_ODOR','FOREIGN_OBJECT','SPOILAGE','PACKAGE_DAMAGE','BATCH_RETURN','OTHER')),
    description TEXT NOT NULL,
    reported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    handled     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_fb_batch ON channel_feedbacks(product_batch_id);

-- ============ 质量事件 ============

CREATE TABLE quality_events (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_no         TEXT NOT NULL UNIQUE,
    type             TEXT NOT NULL
                     CHECK (type IN ('TEMP_EXCEEDED','THAW_TIMEOUT','SEASONING_MISMATCH',
                                     'METAL_ALARM','PACKAGE_LEAK','OFF_ODOR',
                                     'LIVESTREAM_BATCH_RETURN','REGULATOR_RECALL','OTHER')),
    severity         TEXT NOT NULL DEFAULT 'MEDIUM' CHECK (severity IN ('LOW','MEDIUM','HIGH')),
    raw_batch_id     BIGINT REFERENCES raw_material_batches(id),
    product_batch_id BIGINT REFERENCES product_batches(id),
    box_no           TEXT,
    thaw_id          BIGINT REFERENCES thaw_records(id),
    processing_id    BIGINT REFERENCES processing_records(id),
    feedback_id      BIGINT REFERENCES channel_feedbacks(id),
    description      TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','CONFIRMED','DISMISSED','CLOSED')),
    raised_by        TEXT NOT NULL,
    raised_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_by     TEXT,
    confirmed_at     TIMESTAMPTZ,
    conclusion       TEXT,
    recall_id        BIGINT
);
CREATE INDEX idx_qe_status ON quality_events(status);
CREATE INDEX idx_qe_rawbatch ON quality_events(raw_batch_id);
CREATE INDEX idx_qe_productbatch ON quality_events(product_batch_id);

-- ============ 监管抽样 ============

CREATE TABLE regulator_samplings (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sampling_no      TEXT NOT NULL UNIQUE,
    regulator_name   TEXT NOT NULL,
    product_batch_id BIGINT NOT NULL REFERENCES product_batches(id),
    box_id           BIGINT REFERENCES product_boxes(id),
    sample_qty_g     INTEGER NOT NULL,
    purpose          TEXT,
    sampler          TEXT NOT NULL,
    sampled_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    result           TEXT NOT NULL DEFAULT 'PENDING' CHECK (result IN ('PENDING','PASS','FAIL')),
    lab_report_no    TEXT,
    report_at        TIMESTAMPTZ,
    note             TEXT,
    created_event_id BIGINT REFERENCES quality_events(id)
);
CREATE INDEX idx_rs_batch ON regulator_samplings(product_batch_id);

-- ============ 召回 ============

CREATE TABLE recalls (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recall_no        TEXT NOT NULL UNIQUE,
    reason_type      TEXT NOT NULL
                     CHECK (reason_type IN ('TEMP_EXCEEDED','THAW_TIMEOUT','SEASONING_MISMATCH',
                                            'METAL_ALARM','PACKAGE_LEAK','OFF_ODOR',
                                            'LIVESTREAM_BATCH_RETURN','REGULATOR_RECALL','OTHER')),
    level            TEXT NOT NULL DEFAULT 'CLASS_II' CHECK (level IN ('CLASS_I','CLASS_II','CLASS_III')),
    trigger_event_id BIGINT REFERENCES quality_events(id),
    regulator_ref_no TEXT,
    status           TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','RELEASED','CLOSED')),
    description      TEXT NOT NULL,
    initiated_by     TEXT NOT NULL,
    initiated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at      TIMESTAMPTZ,
    released_by      TEXT,
    closed_at        TIMESTAMPTZ
);

CREATE TABLE recall_batches (
    recall_id        BIGINT NOT NULL REFERENCES recalls(id),
    product_batch_id BIGINT NOT NULL REFERENCES product_batches(id),
    action           TEXT NOT NULL DEFAULT 'STOP_SALE_RETURN' CHECK (action IN ('STOP_SALE_ONLY','STOP_SALE_RETURN','DESTROY')),
    PRIMARY KEY (recall_id, product_batch_id)
);

CREATE TABLE recall_documents (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recall_id    BIGINT NOT NULL REFERENCES recalls(id),
    doc_type     TEXT NOT NULL
                 CHECK (doc_type IN ('STOP_SALE_NOTICE','RETURN_NOTICE','COMPENSATION_PLAN',
                                     'SUPPLIER_CLAIM','REGULATOR_REPORT','TRACE_REPORT')),
    target       TEXT NOT NULL DEFAULT '',   -- 渠道/供应商/监管机构名称
    doc_no       TEXT NOT NULL,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL,
    generated_by TEXT NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (recall_id, doc_type, target)
);

-- ============ 退货回厂与分级处置 ============

CREATE TABLE returned_units (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recall_id       BIGINT REFERENCES recalls(id),
    feedback_id     BIGINT REFERENCES channel_feedbacks(id),
    box_id          BIGINT NOT NULL REFERENCES product_boxes(id) UNIQUE,
    source          TEXT NOT NULL CHECK (source IN ('CHANNEL_RETURN','LIVESTREAM_RETURN','WAREHOUSE_RECALL')),
    returned_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    received_by     TEXT NOT NULL,
    decision        TEXT NOT NULL DEFAULT 'PENDING'
                    CHECK (decision IN ('PENDING','RETEST','REWORK','DESTROY')),
    retest_result   TEXT CHECK (retest_result IN ('PENDING','PASS','FAIL')),
    final_status    TEXT NOT NULL DEFAULT 'OPEN'
                    CHECK (final_status IN ('OPEN','RETESTING','RESTOCK_ELIGIBLE','REWORKING','REWORKED','DESTROYED')),
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    handled_at      TIMESTAMPTZ,
    note            TEXT
);
CREATE INDEX idx_ret_recall ON returned_units(recall_id);
CREATE INDEX idx_ret_status ON returned_units(final_status);

-- ============ 供应商追责/扣款 与 客服赔付 ============

CREATE TABLE supplier_claims (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    claim_no      TEXT NOT NULL UNIQUE,
    supplier_id   BIGINT NOT NULL REFERENCES suppliers(id),
    recall_id     BIGINT REFERENCES recalls(id),
    raw_batch_id  BIGINT REFERENCES raw_material_batches(id),
    reason        TEXT NOT NULL,
    claim_amount  NUMERIC(14,2) NOT NULL,
    deduct_amount NUMERIC(14,2) NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'ISSUED'
                  CHECK (status IN ('DRAFT','ISSUED','DEDUCTED','DISPUTED','CLOSED')),
    payment_ref   TEXT,
    issued_by     TEXT NOT NULL,
    issued_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deducted_at   TIMESTAMPTZ
);
CREATE INDEX idx_claim_supplier ON supplier_claims(supplier_id);
CREATE INDEX idx_claim_recall ON supplier_claims(recall_id);

CREATE TABLE customer_compensations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    comp_no     TEXT NOT NULL UNIQUE,
    recall_id   BIGINT REFERENCES recalls(id),
    order_id    BIGINT NOT NULL REFERENCES sales_orders(id),
    channel_id  BIGINT NOT NULL REFERENCES channels(id),
    amount_cny  NUMERIC(14,2) NOT NULL,
    reason      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'PROPOSED' CHECK (status IN ('PROPOSED','PAID','WAIVED')),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at     TIMESTAMPTZ,
    UNIQUE (order_id, recall_id)
);
CREATE INDEX idx_comp_recall ON customer_compensations(recall_id);

-- ============ 冷库盘点 ============

CREATE TABLE stocktakes (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    take_no     TEXT NOT NULL UNIQUE,
    location_id BIGINT NOT NULL REFERENCES storage_locations(id),
    status      TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','CONFIRMED')),
    counted_by  TEXT NOT NULL,
    counted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at TIMESTAMPTZ,
    note        TEXT
);

CREATE TABLE stocktake_lines (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    stocktake_id BIGINT NOT NULL REFERENCES stocktakes(id),
    box_kind     TEXT NOT NULL CHECK (box_kind IN ('RAW','PRODUCT')),
    box_id       BIGINT NOT NULL,
    expected_kg  NUMERIC(10,2) NOT NULL,
    actual_kg    NUMERIC(10,2),
    found        BOOLEAN NOT NULL DEFAULT TRUE,
    variance_kg  NUMERIC(10,2) NOT NULL DEFAULT 0,
    note         TEXT,
    UNIQUE (stocktake_id, box_kind, box_id)
);
CREATE INDEX idx_sl_take ON stocktake_lines(stocktake_id);
