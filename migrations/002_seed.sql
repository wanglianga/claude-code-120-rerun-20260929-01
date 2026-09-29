-- 初始数据：测试账号、基础字典、温控阈值
INSERT INTO users (username, display_name, role, api_token) VALUES
  ('manager', '工厂管理者', 'manager', 'token-manager-001'),
  ('qc',      '品控主管',   'qc',      'token-qc-002'),
  ('op',      '车间操作人', 'operator','token-op-003'),
  ('finance', '财务',       'finance', 'token-finance-004')
ON CONFLICT (username) DO NOTHING;

INSERT INTO materials (code, name, category) VALUES
  ('M-SHRIMP',  '冷冻虾仁', 'SHRIMP'),
  ('M-SCALLOP', '冷冻扇贝', 'SCALLOP'),
  ('M-SQUID',   '冷冻鱿鱼', 'SQUID'),
  ('M-FISH',    '冷冻鱼柳', 'FISH_FILLET')
ON CONFLICT (code) DO NOTHING;

INSERT INTO production_lines (code, name) VALUES
  ('L1', '一号加工线'),
  ('L2', '二号加工线'),
  ('L3', '三号加工线')
ON CONFLICT (code) DO NOTHING;

INSERT INTO locations (code, name, zone, target_temp) VALUES
  ('FZ-A', '冷库A区', 'FREEZER', -22),
  ('FZ-B', '冷库B区', 'FREEZER', -22),
  ('TR-1', '一号解冻间', 'THAW_ROOM', 8),
  ('FC-A', '成品冷库A', 'FINISHED_COLD', -18)
ON CONFLICT (code) DO NOTHING;

INSERT INTO settings (key, value) VALUES
  ('raw_inbound_temp_max_c', '-15'),     -- 冻品入库温度上限
  ('thaw_max_duration_min', '720'),      -- 解冻最长时长 12h
  ('thaw_center_temp_max_c', '10'),      -- 解冻中心温度上限
  ('storage_temp_max_c', '-18'),         -- 冷库温度告警上限
  ('recall_auto_level', 'II')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
