package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) nextDocNo(prefix string) (string, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT nextval('seq_doc')`).Scan(&n); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s%05d", prefix, time.Now().Format("20060102"), n), nil
}

func (s *Server) setting(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	return v
}

func (s *Server) settingFloat(key string, def float64) float64 {
	v := s.setting(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func idFromPath(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (s *Server) getIDByCode(table, codeCol, code string) (int64, error) {
	var id int64
	err := s.db.QueryRow(fmt.Sprintf(`SELECT id FROM %s WHERE code=$1`, table), code).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("%s 编码不存在: %s", table, code)
	}
	return id, err
}

func nullInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

func nullString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// ---------- 供应商 ----------

type supplierReq struct {
	Code                   string `json:"code"`
	Name                   string `json:"name"`
	Contact                string `json:"contact"`
	Phone                  string `json:"phone"`
	QualificationExpiresAt string `json:"qualification_expires_at"`
}

func (s *Server) createSupplier(w http.ResponseWriter, r *http.Request) {
	var req supplierReq
	if !decode(w, r, &req) || req.Code == "" || req.Name == "" {
		errJSON(w, 400, "code 和 name 必填")
		return
	}
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO suppliers(code,name,contact,phone,qualification_expires_at)
		 VALUES ($1,$2,$3,$4,NULLIF($5,'')::date) RETURNING id`,
		req.Code, req.Name, req.Contact, req.Phone, req.QualificationExpiresAt).Scan(&id)
	if err != nil {
		errJSON(w, 409, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code, "name": req.Name})
}

func (s *Server) listSuppliers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id,code,name,contact,phone,qualification_expires_at,status FROM suppliers ORDER BY id`)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	list := scanMaps(rows)
	writeJSON(w, 200, map[string]any{"items": list, "total": len(list)})
}

// ---------- 原料 ----------

type materialReq struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

func (s *Server) createMaterial(w http.ResponseWriter, r *http.Request) {
	var req materialReq
	if !decode(w, r, &req) || req.Code == "" || req.Name == "" || req.Category == "" {
		errJSON(w, 400, "code/name/category 必填")
		return
	}
	var id int64
	err := s.db.QueryRow(`INSERT INTO materials(code,name,category) VALUES ($1,$2,$3) RETURNING id`,
		req.Code, req.Name, req.Category).Scan(&id)
	if err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code})
}

func (s *Server) listMaterials(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT id,code,name,category,unit FROM materials ORDER BY id`)
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

// ---------- 成品 + BOM + 允许调味料 ----------

type productReq struct {
	Code            string             `json:"code"`
	Name            string             `json:"name"`
	DefaultPackSpec string             `json:"default_pack_spec"`
	BOM             []bomItemReq       `json:"bom"`
	Seasonings      []seasoningItemReq `json:"seasonings"`
}
type bomItemReq struct {
	MaterialCode string  `json:"material_code"`
	QtyPerUnit   float64 `json:"qty_per_unit"`
}
type seasoningItemReq struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	var req productReq
	if !decode(w, r, &req) || req.Code == "" || req.Name == "" {
		errJSON(w, 400, "code/name 必填")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	var pid int64
	if err = tx.QueryRow(`INSERT INTO products(code,name,default_pack_spec) VALUES ($1,$2,$3) RETURNING id`,
		req.Code, req.Name, nullString(req.DefaultPackSpec)).Scan(&pid); err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	for _, b := range req.BOM {
		mid, err := lookupID(tx, `SELECT id FROM materials WHERE code=$1`, b.MaterialCode)
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		if b.QtyPerUnit <= 0 {
			errJSON(w, 400, "bom.qty_per_unit 必须大于 0")
			return
		}
		if _, err = tx.Exec(`INSERT INTO product_bom(product_id,material_id,qty_per_unit) VALUES ($1,$2,$3)`,
			pid, mid, b.QtyPerUnit); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
	}
	for _, sn := range req.Seasonings {
		var sid int64
		err = tx.QueryRow(`SELECT id FROM seasonings WHERE code=$1`, sn.Code).Scan(&sid)
		if err == sql.ErrNoRows {
			if sn.Name == "" {
				errJSON(w, 400, "新调味料需提供 name")
				return
			}
			err = tx.QueryRow(`INSERT INTO seasonings(code,name) VALUES ($1,$2) RETURNING id`,
				sn.Code, sn.Name).Scan(&sid)
		}
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		if _, err = tx.Exec(`INSERT INTO product_seasonings(product_id,seasoning_id) VALUES ($1,$2)
			ON CONFLICT DO NOTHING`, pid, sid); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
	}
	if err = tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": pid, "code": req.Code, "name": req.Name})
}

func lookupID(tx *sql.Tx, query, code string) (int64, error) {
	var id int64
	err := tx.QueryRow(query, code).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("编码不存在: %s", code)
	}
	return id, err
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT id,code,name,default_pack_spec FROM products ORDER BY id`)
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

// ---------- 产线 / 库位 ----------

func (s *Server) listLines(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT id,code,name,status FROM production_lines ORDER BY id`)
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

func (s *Server) listLocations(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT id,code,name,zone,target_temp FROM locations ORDER BY id`)
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

// ---------- 渠道 ----------

type channelReq struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Contact string `json:"contact"`
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var req channelReq
	if !decode(w, r, &req) || req.Code == "" || req.Name == "" || req.Type == "" {
		errJSON(w, 400, "code/name/type 必填")
		return
	}
	var id int64
	if err := s.db.QueryRow(`INSERT INTO channels(code,name,type,contact) VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Code, req.Name, req.Type, req.Contact).Scan(&id); err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code})
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT id,code,name,type,contact,status FROM channels ORDER BY id`)
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanMaps(rows)})
}

type packagingReq struct {
	ChannelCode string `json:"channel_code"`
	ProductCode string `json:"product_code"`
	LabelSpec   string `json:"label_spec"`
}

func (s *Server) upsertChannelPackaging(w http.ResponseWriter, r *http.Request) {
	var req packagingReq
	if !decode(w, r, &req) || req.ChannelCode == "" || req.ProductCode == "" || req.LabelSpec == "" {
		errJSON(w, 400, "channel_code/product_code/label_spec 必填")
		return
	}
	cid, err := s.getIDByCode("channels", "code", req.ChannelCode)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	pid, err := s.getIDByCode("products", "code", req.ProductCode)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if _, err = s.db.Exec(`INSERT INTO channel_packagings(channel_id,product_id,label_spec) VALUES ($1,$2,$3)
		ON CONFLICT (channel_id,product_id) DO UPDATE SET label_spec=EXCLUDED.label_spec`,
		cid, pid, req.LabelSpec); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"channel_id": cid, "product_id": pid, "label_spec": req.LabelSpec})
}

// scanMaps 将任意查询结果转为 []map[string]any，按列类型把 NUMERIC 转为数值、
// 时间保持 time.Time，避免 JSON 中数值被当字符串
func scanMaps(rows *sql.Rows) []map[string]any {
	cols, _ := rows.Columns()
	colTypes, _ := rows.ColumnTypes()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		m := map[string]any{}
		for i, c := range cols {
			m[c] = convertCol(vals[i], colTypes[i])
		}
		out = append(out, m)
	}
	return out
}

func convertCol(v any, ct *sql.ColumnType) any {
	if v == nil {
		return nil
	}
	typeName := ""
	if ct != nil {
		typeName = ct.DatabaseTypeName()
	}
	switch x := v.(type) {
	case []byte:
		s := string(x)
		switch typeName {
		case "NUMERIC", "DECIMAL", "FLOAT4", "FLOAT8", "MONEY":
			f, err := strconv.ParseFloat(s, 64)
			if err == nil {
				return f
			}
		case "BOOL":
			return s == "t" || s == "true"
		}
		return s
	case int64:
		return x
	case float64:
		return x
	case bool:
		return x
	case time.Time:
		return x
	default:
		return x
	}
}
