package main

func (a *App) seed() error {
	var n int
	if err := a.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	type userSeed struct{ username, password, role, fullName string }
	users := []userSeed{
		{"admin", "admin123", "admin", "系统管理员"},
		{"qa", "qa123", "qa", "品控经理"},
		{"prod", "prod123", "production", "生产主管"},
		{"wh", "wh123", "warehouse", "冷库仓管"},
		{"sales", "sales123", "sales", "渠道销售"},
		{"finance", "finance123", "finance", "财务专员"},
		{"regulator", "reg123", "regulator", "监管观察员"},
	}
	for _, u := range users {
		h, err := hashPassword(u.password)
		if err != nil {
			return err
		}
		if _, err := a.db.Exec(`INSERT INTO users(username,password_hash,role,full_name) VALUES($1,$2,$3,$4)`,
			u.username, h, u.role, u.fullName); err != nil {
			return err
		}
	}

	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	must := func(q string, args ...any) {
		if err != nil {
			return
		}
		_, err = tx.Exec(q, args...)
	}

	must(`INSERT INTO suppliers(code,name,contact_person,phone,qualification_no) VALUES
		('SUP001','东港远洋渔业有限公司','陈海生','13800000001','QC-2026-0001'),
		('SUP002','蓝海虾业合作社','林阿妹','13800000002','QC-2026-0002')`)
	must(`INSERT INTO channels(code,name,type,contact_person,phone) VALUES
		('CH001','盒海生鲜超市','RETAIL','王采购','13900000001'),
		('CH002','鲜味来连锁餐饮','RESTAURANT','周厨','13900000002'),
		('CH003','海味优选电商旗舰店','ECOMMERCE','吴运营','13900000003'),
		('CH004','爆鲜直播间','LIVESTREAM','郑主播','13900000004')`)
	must(`INSERT INTO production_lines(code,name,workshop) VALUES
		('L1','1号调理线','解冻加工一车间'),
		('L2','2号调理线','解冻加工一车间'),
		('L3','3号油炸线','解冻加工二车间')`)
	must(`INSERT INTO storage_locations(code,name,zone,capacity_kg) VALUES
		('DK-A-01','原料冻库A-01','RAW_FROZEN',5000),
		('DK-A-02','原料冻库A-02','RAW_FROZEN',5000),
		('JD-01','解冻间1号位','THAW_ROOM',1000),
		('CP-B-01','成品冷库B-01','FINISHED_COLD',4000),
		('CP-B-02','成品冷库B-02','FINISHED_COLD',4000),
		('HG-01','隔离待检库HG-01','QUARANTINE',1000),
		('XH-01','销毁暂存区XH-01','DESTROY',500)`)
	must(`INSERT INTO seasoning_batches(batch_no,name,supplier_id,production_date,expiry_date) VALUES
		('SEA-260901','蒜香粉S01',1,'2026-09-01','2027-03-01'),
		('SEA-260910','椒盐粉S02',1,'2026-09-10','2027-03-10'),
		('SEA-260820','奥尔良腌料S03',2,'2026-08-20','2027-02-20')`)
	must(`INSERT INTO packaging_specs(code,name,channel_id,product_code,spec_detail) VALUES
		('PKG-STD','通用中性包装',NULL,NULL,'250g/袋 10袋/箱'),
		('PKG-RT01','盒海定制贴标包装',1,NULL,'250g/袋 盒海LOGO 10袋/箱'),
		('PKG-RS01','餐饮大包装',2,NULL,'1kg/袋 10袋/箱'),
		('PKG-EC01','电商气调锁鲜装',3,NULL,'300g/盒 12盒/箱'),
		('PKG-LV01','直播间家庭分享装',4,NULL,'200g*4袋/箱 直播二维码')`)

	if err != nil {
		return err
	}
	return tx.Commit()
}
