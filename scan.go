package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// queryOneMap 取单行并转为 map
func (a *App) queryOneMap(query string, args ...any) (map[string]any, error) {
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := scanRows(rows)
	if len(items) == 0 {
		return nil, sql.ErrNoRows
	}
	return items[0], nil
}

func atoi64(s string) int64 {
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

// decodeJSONArrays 把 json_agg 返回的 JSON 字符串列解析为真正的数组
func decodeJSONArrays(items []map[string]any, key string) {
	for _, m := range items {
		raw, ok := m[key].(string)
		if !ok {
			continue
		}
		var arr []any
		if json.Unmarshal([]byte(raw), &arr) == nil {
			m[key] = arr
		}
	}
}

// scanRows 把任意查询结果转换成 []map（键为列名），便于直接输出 JSON。
func scanRows(rows *sql.Rows) []map[string]any {
	if rows == nil {
		return []map[string]any{}
	}
	cols, _ := rows.Columns()
	out := make([]map[string]any, 0)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = normalizeVal(vals[i])
		}
		out = append(out, m)
	}
	return out
}

func normalizeVal(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	case nil:
		return nil
	default:
		return t
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 理论上不会失败；失败时用时间戳兜底，保证服务可用
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
