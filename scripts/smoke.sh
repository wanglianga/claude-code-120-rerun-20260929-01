#!/usr/bin/env bash
# 对运行中的服务做端到端冒烟（登录->入库->生产->发货->召回->处置->财务->追溯）。
# 用法：BASE=http://localhost:8080 ./scripts/smoke.sh
set -euo pipefail
BASE="${BASE:-http://localhost:8080}"
C=$(command -v jq >/dev/null && echo jq || echo "")

jget(){ # jget '<json>' key
  python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get(sys.argv[1],""))' "$2" <<<"$1"
}
req(){ # req METHOD PATH TOKEN [JSON]
  local m="$1" p="$2" t="${3:-}" body="${4:-}"
  if [ -n "$body" ]; then
    curl -s -X "$m" "$BASE$p" -H 'Content-Type: application/json' \
      ${t:+-H "Authorization: Bearer $t"} -d "$body"
  else
    curl -s -X "$m" "$BASE$p" ${t:+-H "Authorization: Bearer $t"}
  fi
}

echo "== 健康检查 =="; curl -s "$BASE/healthz"; echo
login(){ req POST /api/v1/auth/login "" "{\"username\":\"$1\",\"password\":\"$2\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])'; }
AT=$(login admin admin123)
QT=$(login qa qa123)
PT=$(login prod prod123)
WT=$(login wh wh123)
ST=$(login sales sales123)
FT=$(login finance finance123)
echo "登录成功（admin/qa/prod/wh/sales/finance）"

TS=$(date +%s)
echo "== 虾仁原料入库（温度合格）=="
RM=$(req POST /api/v1/raw-batches "$WT" "{\"supplier_id\":1,\"material_type\":\"SHRIMP\",\"catch_or_production_date\":\"2026-09-20\",\"inbound_temp_c\":-20,\"quarantine_cert_no\":\"QC-$TS\",\"inspector\":\"wh\",\"boxes\":[{\"box_no\":\"R-$TS-1\",\"weight_kg\":20,\"location_code\":\"DK-A-01\"},{\"box_no\":\"R-$TS-2\",\"weight_kg\":20,\"location_code\":\"DK-A-01\"}]}")
echo "$RM"; RMB=$(jget "$RM" batch_no)

echo "== 下发并开工计划（L1）=="
PLAN=$(req POST /api/v1/production-plans "$PT" "{\"line_code\":\"L1\",\"product_code\":\"P1\",\"product_name\":\"蒜香虾仁\",\"planned_qty_kg\":20,\"expected_seasoning_batch_no\":\"SEA-260901\"}")
PLANNO=$(jget "$PLAN" plan_no); echo "$PLAN"
req POST "/api/v1/production-plans/$PLANNO/start" "$PT" >/dev/null

echo "== 解冻（5 小时前开始，制造超时）=="
THAW=$(req POST /api/v1/thaw-records "$PT" "{\"plan_no\":\"$PLANNO\",\"raw_batch_no\":\"$RMB\",\"water_batch_no\":\"W-$TS\",\"operator\":\"p\",\"raw_weight_kg\":20,\"start_at\":\"$(date -u -d '5 hours ago' +%Y-%m-%dT%H:%M:%SZ)\"}")
TID=$(jget "$THAW" id)
req POST "/api/v1/thaw-records/$TID/finish" "$PT" "{\"center_temp_c\":5}"; echo

for stage in CLEAN CUT SEASON WEIGH SAMPLE COLD_IN; do
  extra="{}"
  [ "$stage" = SEASON ] && extra='{"seasoning_batch_no":"SEA-260910"}'
  [ "$stage" = WEIGH ] && extra='{"weight_kg":18}'
  [ "$stage" = SAMPLE ] && extra='{"sample_qty_g":125}'
  req POST /api/v1/processing-records "$PT" "$(python3 -c "import json,sys;d=json.loads(sys.argv[1]);d.update(plan_no=sys.argv[2],stage=sys.argv[3],operator='p');print(json.dumps(d))" "$extra" "$PLANNO" "$stage")" >/dev/null
done
echo "工序记录完成（SEASON 故意用错调味料批次）"

echo "== 完工生成成品批次（2 箱）=="
PB=$(req POST "/api/v1/production-plans/$PLANNO/complete" "$PT" "{\"cold_location_code\":\"CP-B-01\",\"boxes\":[{\"box_no\":\"P-$TS-1\",\"weight_kg\":9,\"packaging_spec_code\":\"PKG-RT01\"},{\"box_no\":\"P-$TS-2\",\"weight_kg\":9,\"packaging_spec_code\":\"PKG-RT01\"}]}")
echo "$PB"; PBNO=$(jget "$PB" product_batch_no)

echo "== 渠道定制包装发货 + 已售订单 =="
SH=$(req POST /api/v1/shipments "$ST" "{\"channel_code\":\"CH001\",\"product_batch_no\":\"$PBNO\",\"box_nos\":[\"P-$TS-1\",\"P-$TS-2\"],\"packaging_spec_code\":\"PKG-RT01\"}")
echo "$SH"; SHNO=$(jget "$SH" ship_no)
ORD=$(req POST /api/v1/sales-orders "$ST" "{\"channel_code\":\"CH001\",\"ship_no\":\"$SHNO\",\"customer_ref\":\"c-$TS\",\"amount_cny\":600,\"items\":[{\"box_no\":\"P-$TS-1\"},{\"box_no\":\"P-$TS-2\"}]}")
echo "$ORD"; ORDNO=$(jget "$ORD" order_no)

echo "== 渠道反馈异味（自动开质量事件）=="
req POST /api/v1/feedbacks "$ST" "{\"channel_code\":\"CH001\",\"order_no\":\"$ORDNO\",\"product_batch_no\":\"$PBNO\",\"box_no\":\"P-$TS-1\",\"source\":\"CHANNEL\",\"type\":\"OFF_ODOR\",\"description\":\"开袋有异味\"}"; echo

echo "== 按原料批次发起召回并发布 =="
RC=$(req POST /api/v1/recalls "$QT" "{\"reason_type\":\"OFF_ODOR\",\"level\":\"CLASS_II\",\"raw_batch_no\":\"$RMB\",\"description\":\"原料关联异味召回\"}")
RCNO=$(jget "$RC" recall_no); echo "$RC"
req POST "/api/v1/recalls/$RCNO/release" "$QT"; echo

echo "== 退货回厂：复检/返工/销毁 =="
RET=$(req POST /api/v1/returns "$WT" "{\"recall_no\":\"$RCNO\",\"box_no\":\"P-$TS-1\",\"source\":\"CHANNEL_RETURN\",\"received_by\":\"wh\"}")
RID=$(jget "$RET" id)
req POST "/api/v1/returns/$RID/decision" "$QT" '{"decision":"RETEST"}' >/dev/null
req POST "/api/v1/returns/$RID/retest-result" "$QT" '{"result":"PASS"}'; echo "箱1复检合格回库"

echo "== 供应商索赔扣款 + 客户赔付 =="
CL=$(req POST /api/v1/supplier-claims "$FT" "{\"supplier_code\":\"SUP001\",\"recall_no\":\"$RCNO\",\"raw_batch_no\":\"$RMB\",\"reason\":\"原料异味\",\"claim_amount\":5000}")
CLNO=$(jget "$CL" claim_no)
req POST "/api/v1/supplier-claims/$CLNO/deduct" "$FT" '{"deduct_amount":4000,"blacklist_supplier":true}'; echo
CP=$(req POST /api/v1/compensations "$FT" "{\"recall_no\":\"$RCNO\",\"order_no\":\"$ORDNO\",\"amount_cny\":600}")
CPNO=$(jget "$CP" comp_no)
req POST "/api/v1/compensations/$CPNO/pay" "$FT"; echo

echo "== 召回成本 / 谱系 / 看板 =="
req GET "/api/v1/finance/recall-cost/$RCNO" "$FT"; echo
req GET "/api/v1/trace/raw/$RMB" "$QT" | python3 -c 'import sys,json;d=json.load(sys.stdin);print("原料->成品:",[x["batch_no"] for x in d["downstream_products"]])'
req GET /api/v1/dashboard "$AT" | python3 -m json.tool
echo "冒烟完成：召回单 $RCNO，Swagger UI: $BASE/swagger/"
