package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求体格式错误: "+err.Error())
		return
	}
	var (
		hash   string
		u      sessionUser
		active bool
	)
	err := a.db.QueryRow(`SELECT username,password_hash,role,full_name,active FROM users WHERE username=$1`, req.Username).
		Scan(&u.Username, &hash, &u.Role, &u.FullName, &active)
	if err != nil || !checkPassword(hash, req.Password) || !active {
		writeErr(w, 401, "用户名或密码错误")
		return
	}
	tok := randomToken(32)
	raw, _ := json.Marshal(u)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := a.rdb.Set(ctx, "token:"+tok, raw, 12*time.Hour).Err(); err != nil {
		writeErr(w, 500, "会话创建失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "token_type": "Bearer", "expires_in": 43200, "user": u})
}

// ---- 主数据 ----

func (a *App) createSupplier(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code, Name, ContactPerson, Phone, QualificationNo string
	}
	if err := decode(r, &req); err != nil || req.Code == "" || req.Name == "" {
		writeErr(w, 400, "code/name 必填")
		return
	}
	var id int64
	err := a.db.QueryRow(`INSERT INTO suppliers(code,name,contact_person,phone,qualification_no)
		VALUES($1,$2,$3,$4,$5) RETURNING id`, req.Code, req.Name, req.ContactPerson, req.Phone, req.QualificationNo).Scan(&id)
	if err != nil {
		writeErr(w, 409, "创建供应商失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code, "name": req.Name})
}

func (a *App) listSuppliers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,code,name,contact_person,phone,qualification_no,qualified,blacklisted FROM suppliers ORDER BY id`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) createChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code, Name, Type, ContactPerson, Phone string
	}
	if err := decode(r, &req); err != nil || req.Code == "" || req.Name == "" {
		writeErr(w, 400, "code/name/type 必填")
		return
	}
	var id int64
	err := a.db.QueryRow(`INSERT INTO channels(code,name,type,contact_person,phone) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		req.Code, req.Name, req.Type, req.ContactPerson, req.Phone).Scan(&id)
	if err != nil {
		writeErr(w, 409, "创建渠道失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code})
}

func (a *App) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,code,name,type,contact_person,phone,active FROM channels ORDER BY id`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) createLine(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code, Name, Workshop string }
	if err := decode(r, &req); err != nil || req.Code == "" || req.Name == "" {
		writeErr(w, 400, "code/name 必填")
		return
	}
	var id int64
	if err := a.db.QueryRow(`INSERT INTO production_lines(code,name,workshop) VALUES($1,$2,COALESCE($3,'加工车间')) RETURNING id`,
		req.Code, req.Name, req.Workshop).Scan(&id); err != nil {
		writeErr(w, 409, "创建产线失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code})
}

func (a *App) listLines(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,code,name,workshop,active FROM production_lines ORDER BY id`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) createLocation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code, Name, Zone string
		CapacityKg       float64
	}
	if err := decode(r, &req); err != nil || req.Code == "" || req.Name == "" || req.Zone == "" {
		writeErr(w, 400, "code/name/zone 必填")
		return
	}
	var id int64
	if err := a.db.QueryRow(`INSERT INTO storage_locations(code,name,zone,capacity_kg) VALUES($1,$2,$3,NULLIF($4,0)) RETURNING id`,
		req.Code, req.Name, req.Zone, req.CapacityKg).Scan(&id); err != nil {
		writeErr(w, 409, "创建库位失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code})
}

func (a *App) listLocations(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,code,name,zone,capacity_kg,active FROM storage_locations ORDER BY zone,code`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) createSeasoningBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BatchNo, Name              string
		SupplierID                 int64
		ProductionDate, ExpiryDate string
	}
	if err := decode(r, &req); err != nil || req.BatchNo == "" || req.Name == "" {
		writeErr(w, 400, "batch_no/name 必填")
		return
	}
	var id int64
	err := a.db.QueryRow(`INSERT INTO seasoning_batches(batch_no,name,supplier_id,production_date,expiry_date)
		VALUES($1,$2,NULLIF($3,0),NULLIF($4,'')::date,NULLIF($5,'')::date) RETURNING id`,
		req.BatchNo, req.Name, req.SupplierID, req.ProductionDate, req.ExpiryDate).Scan(&id)
	if err != nil {
		writeErr(w, 409, "创建调味料批次失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "batch_no": req.BatchNo})
}

func (a *App) listSeasoningBatches(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,batch_no,name,supplier_id,production_date,expiry_date FROM seasoning_batches ORDER BY id DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) createPackagingSpec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code, Name              string
		ChannelID               int64
		ProductCode, SpecDetail string
	}
	if err := decode(r, &req); err != nil || req.Code == "" || req.Name == "" {
		writeErr(w, 400, "code/name 必填")
		return
	}
	var id int64
	err := a.db.QueryRow(`INSERT INTO packaging_specs(code,name,channel_id,product_code,spec_detail)
		VALUES($1,$2,NULLIF($3,0),$4,$5) RETURNING id`,
		req.Code, req.Name, req.ChannelID, req.ProductCode, req.SpecDetail).Scan(&id)
	if err != nil {
		writeErr(w, 409, "创建包装规格失败: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "code": req.Code})
}

func (a *App) listPackagingSpecs(w http.ResponseWriter, r *http.Request) {
	q := `SELECT p.id,p.code,p.name,p.channel_id,COALESCE(c.name,'通用') AS channel_name,p.product_code,p.spec_detail
		FROM packaging_specs p LEFT JOIN channels c ON c.id=p.channel_id ORDER BY p.id`
	rows, err := a.db.Query(q)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) getConfig(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT key,value,description FROM system_config ORDER BY key`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, map[string]any{"items": scanRows(rows)})
}

func (a *App) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := decode(r, &req); err != nil || len(req) == 0 {
		writeErr(w, 400, "请传入要更新的键值")
		return
	}
	for k, v := range req {
		_, err := a.db.Exec(`INSERT INTO system_config(key,value,description) VALUES($1,$2,'')
			ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, k, v)
		if err != nil {
			writeErr(w, 400, "更新失败 "+k+": "+err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]any{"updated": len(req)})
}
