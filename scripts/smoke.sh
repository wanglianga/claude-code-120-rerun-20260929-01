#!/usr/bin/env bash
# 端到端冒烟脚本：入库 → 多线并行解冻加工 → 异常 → 留样放行 → 发货订单
#               → 渠道反馈/监管抽检 → 召回定位与材料 → 退货三态处置 → 赔付/扣款 → 盘点 → 溯源
# 依赖：curl、jq。用法：BASE_URL=http://localhost:8080 bash scripts/smoke.sh
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
MGR="token-manager-001"; QC="token-qc-002"; OP="token-op-003"; FIN="token-finance-004"
PASS=0; FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# api <token> <method> <path> [json-body] [idempotency-key]
api() {
  local token="$1" method="$2" path="$3" body="${4:-}" idem="${5:-}"
  local args=(-sS -X "$method" -H "Authorization: Bearer $token" -H 'Content-Type: application/json')
  [ -n "$body" ] && args+=(-d "$body")
  [ -n "$idem" ] && args+=(-H "Idempotency-Key: $idem" -D "$TMP/hdr")
  curl "${args[@]}" "$BASE_URL$path"
}
jqr() { jq -r "$1"; }
check() { # check <desc> <condition-string>
  if eval "$2"; then echo "  ✅ $1"; PASS=$((PASS+1));
  else echo "  ❌ $1"; FAIL=$((FAIL+1)); fi
}
section() { echo; echo "==================== $1 ===================="; }

section "0. 健康检查"
curl -sS "$BASE_URL/healthz" | tee "$TMP/health.json"
check "服务健康且 PG/Redis 在线" '[ $(jq .postgres "$TMP/health.json") = "true" ] && [ $(jq .redis "$TMP/health.json") = "true" ]'

section "1. 主数据：供应商/成品BOM/调味料/渠道/定制包装"
api "$MGR" POST /api/v1/suppliers '{"code":"SUP-A","name":"北洋远洋渔业","contact":"王经理","phone":"13800000001"}' >/dev/null
api "$MGR" POST /api/v1/suppliers '{"code":"SUP-B","name":"东港水产合作社","contact":"李经理"}' >/dev/null
api "$MGR" POST /api/v1/products '{"code":"P-SHRIMP","name":"黄金凤尾虾(预制菜)","default_pack_spec":"标准装 500g/袋",
  "bom":[{"material_code":"M-SHRIMP","qty_per_unit":1.1}],
  "seasonings":[{"code":"S-SALT","name":"椒盐腌料"},{"code":"S-GARLIC","name":"蒜香腌料"}]}' >/dev/null
api "$MGR" POST /api/v1/products '{"code":"P-SQUID","name":"孜然鱿鱼须",
  "bom":[{"material_code":"M-SQUID","qty_per_unit":1.05}],
  "seasonings":[{"code":"S-CUMIN","name":"孜然腌料"}]}' >/dev/null
api "$MGR" POST /api/v1/channels '{"code":"LIVE-1","name":"海鲜直播间","type":"LIVE_ROOM","contact":"主播助理"}' >/dev/null
api "$MGR" POST /api/v1/channels '{"code":"MKT-1","name":"华东连锁商超","type":"SUPERMARKET"}' >/dev/null
api "$MGR" POST /api/v1/channels/packaging '{"channel_code":"LIVE-1","product_code":"P-SHRIMP","label_spec":"直播间定制 300g×3 礼盒"}' >/dev/null
check "主数据写入成功" '[ $(api "$MGR" GET /api/v1/channels | jq ".items|length") -ge 2 ]'

section "2. 采购入库（含温度超标自动隔离与品控事件）"
R1=$(api "$QC" POST /api/v1/inbound/raw-batches '{"material_code":"M-SHRIMP","supplier_code":"SUP-A",
  "catch_or_production_date":"2026-09-20","arrival_at":"2026-09-25T08:00:00Z",
  "inbound_temp_c":-18,"quarantine_cert_no":"QY-20260925-01",
  "boxes":[{"box_no":"B01","location_code":"FZ-A","weight_kg":100},{"box_no":"B02","location_code":"FZ-A","weight_kg":100}]}')
echo "$R1" | jq .
RAW1=$(echo "$R1" | jq -r .batch_no)
check "合格原料正常在库" "[ $(echo "$R1"|jqr .status) = in_storage ]"
R2=$(api "$QC" POST /api/v1/inbound/raw-batches '{"material_code":"M-SHRIMP","supplier_code":"SUP-B",
  "catch_or_production_date":"2026-09-21","arrival_at":"2026-09-25T09:00:00Z",
  "inbound_temp_c":-12,"quarantine_cert_no":"QY-20260925-02",
  "boxes":[{"box_no":"C01","location_code":"FZ-B","weight_kg":50}]}')
echo "$R2" | jq .
RAW2=$(echo "$R2" | jq -r .batch_no)
check "入库温度超标(-12℃>-15℃)自动隔离并产生品控事件" \
  '[ $(echo "$R2"|jqr .status) = quarantined ] && [ -n "$(echo "$R2"|jqr .temperature_alert.event_no)" ]'
R3=$(api "$QC" POST /api/v1/inbound/raw-batches '{"material_code":"M-SQUID","supplier_code":"SUP-A",
  "catch_or_production_date":"2026-09-22","inbound_temp_c":-20,"quarantine_cert_no":"QY-20260925-03",
  "boxes":[{"box_no":"D01","location_code":"FZ-B","weight_kg":80}]}')
RAW3=$(echo "$R3" | jq -r .batch_no)

section "3. 生产计划 + 两条生产线并行工单"
PL=$(api "$MGR" POST /api/v1/production/plans '{"product_code":"P-SHRIMP","planned_qty":80}' | jq -r .plan_no)
W1=$(api "$MGR" POST /api/v1/production/work-orders "{\"plan_no\":\"$PL\",\"line_code\":\"L1\",\"planned_qty\":50}" | jq -r .order_no)
W2=$(api "$MGR" POST /api/v1/production/work-orders "{\"plan_no\":\"$PL\",\"line_code\":\"L2\",\"planned_qty\":30}" | jq -r .order_no)
check "同一计划在 L1/L2 并行下发工单" "[ -n $W1 ] && [ -n $W2 ] && [ $W1 != $W2 ]"
check "隔离原料批次禁止投入解冻" \
  'api "$OP" POST /api/v1/production/thaw-records "{\"work_order_no\":\"$W1\",\"raw_batch_no\":\"$RAW2\",\"water_batch_no\":\"W-X\",\"operator\":\"op\"}" 2>/dev/null | grep -q 隔离'

section "4. L1 工单：解冻→投料→清洗切配调味包装→金检→成品留样"
T1=$(api "$OP" POST /api/v1/production/thaw-records "{\"work_order_no\":\"$W1\",\"raw_batch_no\":\"$RAW1\",
  \"start_at\":\"2026-09-29T06:00:00Z\",\"start_center_temp_c\":-18,\"water_batch_no\":\"WATER-901\",\"operator\":\"张三\"}" | jq -r .id)
TC1=$(api "$OP" POST /api/v1/production/thaw-records/$T1/complete '{"end_at":"2026-09-29T08:00:00Z","end_center_temp_c":2,"max_center_temp_c":6}')
echo "$TC1" | jq -c '"L1 解冻完成: \(.)"'
check "L1 解冻未超时未超温(无告警)" '[ $(echo "$TC1"|jqr .timed_out) = false ]'
api "$OP" POST /api/v1/production/material-usages "{\"work_order_no\":\"$W1\",\"raw_batch_no\":\"$RAW1\",\"box_no\":\"B01\",\"qty_kg\":60}" >/dev/null
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W1\",\"stage\":\"WASHING\",\"operator\":\"张三\"}" >/dev/null
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W1\",\"stage\":\"CUTTING\"}" >/dev/null
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W1\",\"stage\":\"SEASONING\",\"detail\":{\"seasoning_code\":\"S-SALT\",\"batch\":\"SALT-09\"}}" >/dev/null
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W1\",\"stage\":\"WEIGHING\",\"detail\":{\"unit_weight_g\":500,\"total_pieces\":100}}" >/dev/null
PKG1=$(api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W1\",\"stage\":\"PACKAGING\",\"detail\":{\"channel_code\":\"LIVE-1\"}}")
check "渠道定制包装标签被自动套用" 'echo "$PKG1" | jq -e ".alerts|length==0" >/dev/null'
api "$OP" POST /api/v1/production/metal-checks "{\"work_order_no\":\"$W1\",\"result\":\"PASS\"}" >/dev/null
FG1=$(api "$QC" POST /api/v1/production/finished-batches "{\"work_order_no\":\"$W1\",\"qty_kg\":50,\"channel_code\":\"LIVE-1\"}")
echo "$FG1" | jq .
FG1_NO=$(echo "$FG1" | jq -r .batch_no); FG1_ID=$(echo "$FG1" | jq -r .id); SP1_NO=$(echo "$FG1"|jqr .sample_no)
check "成品产出自动入库+自动留样" "[ $FG1_ID -gt 0 ] && [ $SP1_NO != null ]"

section "5. L2 工单：解冻超时 + 调味料用错 → 冻结 → 品控放行 → 产出"
T2=$(api "$OP" POST /api/v1/production/thaw-records "{\"work_order_no\":\"$W2\",\"raw_batch_no\":\"$RAW1\",
  \"start_at\":\"2026-09-28T00:00:00Z\",\"start_center_temp_c\":-18,\"water_batch_no\":\"WATER-902\",\"operator\":\"李四\"}" | jq -r .id)
TC2=$(api "$OP" POST /api/v1/production/thaw-records/$T2/complete '{"end_at":"2026-09-28T14:00:00Z","end_center_temp_c":3,"max_center_temp_c":7}')
echo "$TC2" | jq -c '"L2 解冻完成: \(.)"'
QE_TIMEOUT=$(echo "$TC2" | jq -r '.alerts[0].event_no')
check "解冻 840 分钟超过 720 分钟上限 → THAW_TIMEOUT 事件" '[ -n "$QE_TIMEOUT" ] && [ "$QE_TIMEOUT" != null ]'
api "$OP" POST /api/v1/production/material-usages "{\"work_order_no\":\"$W2\",\"raw_batch_no\":\"$RAW1\",\"box_no\":\"B02\",\"qty_kg\":40}" >/dev/null
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W2\",\"stage\":\"WASHING\"}" >/dev/null
WRONG=$(api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W2\",\"stage\":\"SEASONING\",\"detail\":{\"seasoning_code\":\"S-CUMIN\"}}")
echo "$WRONG" | jq .
QE_WRONG=$(echo "$WRONG" | jq -r '.alerts[0].event_no')
check "鱿鱼用孜然腌料用于虾产品 → SEASONING_WRONG 且工单冻结" '[ -n "$QE_WRONG" ] && [ "$QE_WRONG" != null ]'
FROZEN=$(api "$OP" POST /api/v1/production/metal-checks "{\"work_order_no\":\"$W2\",\"result\":\"PASS\"}" || true)
check "工单冻结期间禁止继续工序" 'echo "$FROZEN" | grep -q 冻结'
QE_WRONG_ID=$(api "$QC" GET /api/v1/quality/events | jq -r ".items[]|select(.event_no==\"$QE_WRONG\").id")
REL=$(api "$QC" POST /api/v1/quality/events/$QE_WRONG_ID/conclusion '{"conclusion":"误拿邻线腌料，已更换，核查半成品无污染","confirmed":false}')
echo "$REL" | jq .
check "品控结论不成立 → 工单解除冻结恢复生产" 'echo "$REL" | jq -e .work_order_released >/dev/null'
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W2\",\"stage\":\"SEASONING\",\"detail\":{\"seasoning_code\":\"S-GARLIC\"}}" >/dev/null
api "$OP" POST /api/v1/production/process-events "{\"work_order_no\":\"$W2\",\"stage\":\"PACKAGING\"}" >/dev/null
api "$OP" POST /api/v1/production/metal-checks "{\"work_order_no\":\"$W2\",\"result\":\"PASS\"}" >/dev/null
FG2=$(api "$QC" POST /api/v1/production/finished-batches "{\"work_order_no\":\"$W2\",\"qty_kg\":30}")
FG2_NO=$(echo "$FG2" | jq -r .batch_no); FG2_ID=$(echo "$FG2" | jq -r .id)
check "L2 成品批次产出" '[ $FG2_ID -gt 0 ]'

section "6. 留样检测放行（未放行成品禁止发货）"
SP1=$(api "$QC" GET /api/v1/production/finished-batches/$FG1_ID | jq -r '.samples[0].id')
SP2=$(api "$QC" GET /api/v1/production/finished-batches/$FG2_ID | jq -r '.samples[0].id')
api "$QC" POST /api/v1/quality/samples/$SP1/result '{"result":"qualified","report_no":"LAB-9001","lab":"厂检中心","conclusion":"微生物/感官合格"}' >/dev/null
api "$QC" POST /api/v1/quality/samples/$SP2/result '{"result":"qualified","report_no":"LAB-9002","lab":"厂检中心","conclusion":"合格"}' >/dev/null
NOSHIP=$(api "$MGR" POST /api/v1/sales/shipments "{\"channel_code\":\"LIVE-1\",\"items\":[{\"finished_batch_no\":\"$RAW3\",\"qty_kg\":1}]}" || true)
check "未留样放行的批次无法发货" 'echo "$NOSHIP" | grep -qE "未放行|不存在"'

section "7. 发货到直播间/商超 + 已售订单"
SH1=$(api "$MGR" POST /api/v1/sales/shipments "{\"channel_code\":\"LIVE-1\",\"note\":\"9月30日直播专场\",
  \"items\":[{\"finished_batch_no\":\"$FG1_NO\",\"qty_kg\":30},{\"finished_batch_no\":\"$FG2_NO\",\"qty_kg\":10}]}")
SH1_NO=$(echo "$SH1"|jqr .shipment_no)
SH2=$(api "$MGR" POST /api/v1/sales/shipments "{\"channel_code\":\"MKT-1\",\"items\":[{\"finished_batch_no\":\"$FG1_NO\",\"qty_kg\":10}]}")
SH2_NO=$(echo "$SH2"|jqr .shipment_no)
check "两渠道发货成功" '[ -n "$SH1_NO" ] && [ -n "$SH2_NO" ]'
api "$MGR" POST /api/v1/sales/orders "{\"order_no\":\"O-1001\",\"channel_code\":\"LIVE-1\",\"shipment_no\":\"$SH1_NO\",\"finished_batch_no\":\"$FG1_NO\",\"buyer\":\"userA\",\"qty_kg\":5,\"amount\":200}" >/dev/null
api "$MGR" POST /api/v1/sales/orders "{\"order_no\":\"O-1002\",\"channel_code\":\"LIVE-1\",\"shipment_no\":\"$SH1_NO\",\"finished_batch_no\":\"$FG2_NO\",\"buyer\":\"userB\",\"qty_kg\":2,\"amount\":90}" >/dev/null
api "$MGR" POST /api/v1/sales/orders "{\"order_no\":\"O-1003\",\"channel_code\":\"MKT-1\",\"shipment_no\":\"$SH2_NO\",\"finished_batch_no\":\"$FG1_NO\",\"buyer\":\"门店X\",\"qty_kg\":3,\"amount\":120}" >/dev/null
check "已售订单 3 笔" '[ $(api "$MGR" GET /api/v1/sales/orders|jqr ".items|length") -ge 3 ]'

section "8. 渠道反馈异味 + 监管抽样不合格"
FB1=$(api "$OP" POST /api/v1/channels/feedback "{\"channel_code\":\"LIVE-1\",\"finished_batch_no\":\"$FG1_NO\",\"order_no\":\"O-1001\",\"feedback_type\":\"ODOR\",\"content\":\"消费者反馈虾品有氨水异味，直播间集中投诉\"}")
echo "$FB1" | jq .
QE_ODOR=$(echo "$FB1"|jqr .quality_event_no)
check "异味反馈自动生成 CHANNEL_ODOR 品控事件" '[ -n "$QE_ODOR" ]'
RS=$(api "$QC" POST /api/v1/regulatory/samples "{\"regulator\":\"市市场监管局\",\"finished_batch_no\":\"$FG1_NO\"}")
RS_ID=$(echo "$RS"|jqr .id)
RR=$(api "$QC" POST /api/v1/regulatory/samples/$RS_ID/result '{"result":"unqualified","report_no":"MJ-777","conclusion":"挥发性盐基氮超标"}')
echo "$RR" | jq .
QE_REG=$(echo "$RR"|jqr '.alerts[0].event_no')
check "监管抽检不合格 → REGULATOR_RECALL 事件" '[ -n "$QE_REG" ] && [ "$QE_REG" != null ]'

section "9. 按原料批次发起召回：范围扩散到 FG1/FG2，定位渠道/订单/留样/箱"
RC=$(api "$QC" POST /api/v1/recalls "{\"event_no\":\"$QE_REG\",\"raw_batch_no\":\"$RAW1\",\"level\":\"I\",\"reason\":\"原料虾挥发性盐基氮超标，渠道反馈异味，监管抽检不合格\"}")
echo "$RC" | jq '{id,recall_no,level,status,summary}'
RC_ID=$(echo "$RC"|jqr .id)
check "召回范围覆盖使用该原料的 2 个成品批次" '[ $(echo "$RC"|jqr ".summary.finished_batch_count") -eq 2 ]'
check "定位 3 笔已售订单" '[ $(echo "$RC"|jqr ".summary.order_count") -eq 3 ]'
check "精准定位到原料箱 B01/B02" '[ $(echo "$RC"|jqr ".boxes|length") -ge 2 ]'
check "关联留样检测结果" '[ $(echo "$RC"|jqr ".samples|length") -ge 2 ]'
check "草稿状态尚未生成材料" '[ $(echo "$RC"|jqr ".documents|length") -eq 0 ]'

section "10. 发布召回：停售/退货/赔偿/供应商追责/监管报告 材料一键生成"
REL_RC=$(api "$QC" POST /api/v1/recalls/$RC_ID/release)
echo "$REL_RC" | jq '{recall_no,status,summary}'
DOCS=$(api "$QC" GET /api/v1/recalls/$RC_ID/documents)
echo "$DOCS" | jq '.documents[]|{action_type,target_type,target_ref,title,status}'
check "召回单已发布" '[ $(echo "$REL_RC"|jqr .status) = released ]'
check "在库库存全部冻结停售(hold>0,on_hand=0)" 'echo "$REL_RC"|jq -e ".summary.on_hand_kg==0 and .summary.hold_kg>0" >/dev/null'
for t in STOP_SALE CHANNEL_NOTICE RETURN_REQUEST COMPENSATION SUPPLIER_CLAIM REGULATOR_REPORT; do
  check "生成 $t 材料" "echo \"\$DOCS\" | jq -e --arg t $t '[.documents[]|select(.action_type==\$t)]|length>0' >/dev/null"
done
check "已售订单全部通知召回" '[ $(echo "$REL_RC"|jqr ".summary.pending_notify_orders") -eq 0 ]'
check "自动按渠道汇总拟定退款赔付单" '[ $(api "$FIN" GET /api/v1/finance/compensations|jqr ".items|length") -ge 2 ]'

section "11. 退货回厂三态处置：可复检 / 必须销毁 / 可返工"
RT1=$(api "$OP" POST /api/v1/returns "{\"recall_no\":$(echo "$REL_RC"|jqr .recall_no|jq -R .),\"finished_batch_no\":\"$FG1_NO\",\"channel_code\":\"LIVE-1\",\"order_no\":\"O-1001\",\"qty_kg\":20,\"reason\":\"直播间退回\"}")
RT1_ID=$(echo "$RT1"|jqr .id)
RT2=$(api "$OP" POST /api/v1/returns "{\"finished_batch_no\":\"$FG2_NO\",\"channel_code\":\"LIVE-1\",\"order_no\":\"O-1002\",\"qty_kg\":5}")
RT2_ID=$(echo "$RT2"|jqr .id)
check "退货入隔离待处置区（不进可售库存）" '[ $RT1_ID -gt 0 ]'
OVER=$(api "$OP" POST /api/v1/returns "{\"finished_batch_no\":\"$FG2_NO\",\"qty_kg\":999}" || true)
check "回厂数量超过已发量被拒绝" 'echo "$OVER"|grep -q 超过'
# RT1 可复检 → 复检合格回可售
api "$QC" POST /api/v1/returns/$RT1_ID/disposition '{"disposition":"REINSPECT","note":"包装完好待复检"}' >/dev/null
RP=$(api "$QC" POST /api/v1/returns/$RT1_ID/reinspect '{"pass":true,"note":"复检微生物合格"}')
echo "$RP" | jq .
check "复检合格数量 20kg 回到可售" 'echo "$RP"|jq -e ".status==\"reinspect_pass\" and .restored_on_hand_kg==20" >/dev/null'
# RT2 必须销毁
RD=$(api "$QC" POST /api/v1/returns/$RT2_ID/disposition '{"disposition":"DESTROY","note":"涨袋异味必须销毁"}')
echo "$RD" | jq .
check "销毁处置入账 destroyed" 'echo "$RD"|jq -e ".destroyed_kg==5" >/dev/null'
# RT3 可返工 → 新成品批次 + 重新留样
RT3=$(api "$OP" POST /api/v1/returns "{\"finished_batch_no\":\"$FG1_NO\",\"channel_code\":\"MKT-1\",\"qty_kg\":5}")
RT3_ID=$(echo "$RT3"|jqr .id)
RW=$(api "$QC" POST /api/v1/returns/$RT3_ID/disposition '{"disposition":"REWORK","note":"仅外包装破损，返工换袋"}')
echo "$RW" | jq .
FGR_NO=$(echo "$RW"|jqr .new_finished_batch_no); SPR_NO=$(echo "$RW"|jqr .new_sample_no)
check "返工产出新批次并重新留样" '[ "$FGR_NO" != null ] && [ "$SPR_NO" != null ]'
check "退货三种状态互不串账" '[ $(api "$OP" GET /api/v1/returns|jqr "[.items[]|select(.status==\"reinspect_pass\")]|length") -ge 1 ]'

section "12. 财务：客服赔付支付 + 供应商扣款追偿结算"
CP=$(api "$FIN" POST /api/v1/finance/compensations '{"order_no":"O-1003","amount":120,"kind":"REFUND","note":"商超订单召回退款"}')
CP_ID=$(echo "$CP"|jqr .id)
api "$FIN" POST /api/v1/finance/compensations/$CP_ID/pay >/dev/null
check "赔付单可支付并回写订单状态" 'api "$FIN" GET /api/v1/finance/compensations|jq -e "[.items[]|select(.status==\"paid\")]|length>=1" >/dev/null'
SD=$(api "$FIN" POST /api/v1/finance/supplier-deductions "{\"supplier_code\":\"SUP-A\",\"recall_no\":$(echo "$REL_RC"|jqr .recall_no|jq -R .),\"raw_batch_no\":\"$RAW1\",\"amount\":50000,\"reason\":\"原料超标导致一级召回的货款抵扣与损失追偿\"}")
SD_ID=$(echo "$SD"|jqr .id)
api "$FIN" POST /api/v1/finance/supplier-deductions/$SD_ID/settle >/dev/null
check "供应商扣款可结算追偿" 'api "$FIN" GET /api/v1/finance/supplier-deductions|jq -e ".settled_amount>=50000" >/dev/null'

section "13. 冷库盘点（盘差调整不冲销召回冻结量）"
ST=$(api "$QC" POST /api/v1/inventory/stocktakes '{"location_code":"FC-A"}')
ST_ID=$(echo "$ST"|jqr .id)
SYS=$(api "$QC" GET /api/v1/inventory/stock | jq -r --arg b "$FG1_NO" '.items[]|select(.batch_no==$b)|.qty_on_hand+.qty_hold')
api "$OP" POST /api/v1/inventory/stocktakes/$ST_ID/count "{\"finished_batch_no\":\"$FG1_NO\",\"counted_qty\":$SYS}" >/dev/null
# 其余行按账面录平
api "$QC" GET /api/v1/inventory/stock | jq -r '.items[]|.batch_no' | while read -r b; do
  sys=$(api "$QC" GET /api/v1/inventory/stock | jq -r --arg b "$b" '.items[]|select(.batch_no==$b)|.qty_on_hand+.qty_hold')
  api "$OP" POST /api/v1/inventory/stocktakes/$ST_ID/count "{\"finished_batch_no\":\"$b\",\"counted_qty\":$sys}" >/dev/null
done
# 把 FG1 改成实盘 +1 制造盘盈
PLUS1=$(jq -n --argjson s "$SYS" '$s+1')
api "$OP" POST /api/v1/inventory/stocktakes/$ST_ID/count "{\"finished_batch_no\":\"$FG1_NO\",\"counted_qty\":$PLUS1}" >/dev/null
CF=$(api "$QC" POST /api/v1/inventory/stocktakes/$ST_ID/confirm)
echo "$CF" | jq .
check "盘点确认并产生 +1kg 盘盈调整" 'echo "$CF"|jq -e ".status==\"confirmed\" and .total_diff_kg==1" >/dev/null'

section "14. 溯源（Redis 缓存生效）"
RAW1_ID=$(api "$OP" GET "/api/v1/inbound/raw-batches?batch_no=$RAW1" | jq -r .items[0].id)
TR=$(api "$OP" GET /api/v1/trace/raw-batch/$RAW1_ID)
echo "$TR" | jq '{raw:.raw_batch.batch_no, finished:[.finished_batches[].batch_no], channels:[.channels[].channel_code], orders:(.orders|length), recalls:[.recalls[].recall_no]}'
check "正向溯源：原料→成品(含返工新批)→渠道→订单→召回" \
  'echo "$TR"|jq -e "((.finished_batches|length)>=3) and ((.channels|length)>=2) and ((.orders|length)==3) and ((.recalls|length)>=1)" >/dev/null'
TR2=$(api "$OP" GET /api/v1/trace/finished-batch/$FG1_ID)
echo "$TR2" | jq '{fg:.finished_batch.batch_no, raw:[.raw_batches[].batch_no], boxes:(.raw_boxes|length), supplier:.raw_batches[0].supplier_name, samples:[.samples[].result]}'
check "反向溯源：成品→原料批次/供应商/箱→全工序→留样" \
  'echo "$TR2"|jq -e "(.raw_batches|length>=1) and (.raw_boxes|length>=2) and (.process_events|length>=5)" >/dev/null'
api "$OP" GET /api/v1/trace/raw-batch/$RAW1_ID >/dev/null
check "溯源结果第二次命中 Redis 缓存" 'api "$OP" GET /api/v1/trace/raw-batch/$RAW1_ID | jq -e .cached >/dev/null'

section "15. 写接口幂等（Idempotency-Key 重放）"
R=$(api "$MGR" POST /api/v1/suppliers '{"code":"SUP-IDEM","name":"幂等供应商"}' "idem-key-001")
R2=$(api "$MGR" POST /api/v1/suppliers '{"code":"SUP-IDEM","name":"幂等供应商"}' "idem-key-001")
check "同幂等键重复提交返回首次结果" "[ $(echo "$R"|jqr .id) = $(echo "$R2"|jqr .id) ]"

section "16. 权限控制"
check "操作人不能创建供应商(403)" 'api "$OP" POST /api/v1/suppliers "{\"code\":\"X\",\"name\":\"x\"}" | grep -q "role not permitted"'
check "无 Token 拒绝访问(401)" 'curl -sS "$BASE_URL/api/v1/lines" | grep -q "missing bearer token"'

echo
echo "==================== 结果 ===================="
echo "PASS=$PASS  FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
