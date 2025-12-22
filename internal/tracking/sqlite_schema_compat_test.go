package tracking

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 回归测试：旧库 request_logs 缺少新列（例如 auth_key）时，InitSchema 不应直接因 schema.sql 中索引创建失败而报错。
func TestSQLiteAdapter_InitSchema_LegacyDBMissingColumns(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	dsn := dbPath + "?_journal_mode=WAL&_foreign_keys=1&_busy_timeout=60000"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// 1) 造一个“旧版本”数据库：request_logs/endpoints/channels 存在，但缺少后续新增列（auth_key、缓存字段等）。
	// 注意：旧表存在时，schema.sql 的 CREATE TABLE IF NOT EXISTS 不会补列，随后 CREATE INDEX(auth_key) 会报错。
	_, err = db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS request_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT UNIQUE NOT NULL,
    client_ip TEXT,
    user_agent TEXT,
    method TEXT DEFAULT 'POST',
    path TEXT DEFAULT '/v1/messages',
    start_time DATETIME NOT NULL,
    end_time DATETIME,
    duration_ms INTEGER,
    channel TEXT DEFAULT '',
    endpoint_name TEXT,
    group_name TEXT,
    model_name TEXT,
    is_streaming BOOLEAN DEFAULT FALSE,
    status TEXT NOT NULL DEFAULT 'pending',
    http_status_code INTEGER,
    retry_count INTEGER DEFAULT 0,
    failure_reason TEXT,
    last_failure_reason TEXT,
    cancel_reason TEXT,
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    cache_creation_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    input_cost_usd REAL DEFAULT 0,
    output_cost_usd REAL DEFAULT 0,
    cache_creation_cost_usd REAL DEFAULT 0,
    cache_read_cost_usd REAL DEFAULT 0,
    total_cost_usd REAL DEFAULT 0,
    created_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') || '+08:00'),
    updated_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') || '+08:00')
);

CREATE TABLE IF NOT EXISTS endpoints (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel TEXT NOT NULL,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    token TEXT,
    api_key TEXT,
    headers TEXT,
    priority INTEGER DEFAULT 1,
    failover_enabled INTEGER DEFAULT 1,
    cooldown_seconds INTEGER,
    enabled INTEGER DEFAULT 1,
    created_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') || '+08:00'),
    updated_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') || '+08:00')
);

CREATE TABLE IF NOT EXISTS channels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    website TEXT,
    created_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') || '+08:00'),
    updated_at DATETIME DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now', 'localtime') || '+08:00')
);
`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	adapter, err := NewSQLiteAdapter(DatabaseConfig{
		Type:         "sqlite",
		DatabasePath: dbPath,
		Timezone:     "Asia/Shanghai",
	})
	if err != nil {
		t.Fatalf("NewSQLiteAdapter: %v", err)
	}
	if err := adapter.Open(); err != nil {
		t.Fatalf("adapter.Open: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	// 2) InitSchema 需要自动补列，并确保 schema.sql 可顺利执行（包括索引/触发器）。
	if err := adapter.InitSchema(); err != nil {
		t.Fatalf("adapter.InitSchema: %v", err)
	}

	// 3) 验证关键列/表已存在
	if !sqliteColumnExists(t, adapter.db, "request_logs", "auth_key") {
		t.Fatalf("expected request_logs.auth_key to exist after InitSchema")
	}
	if !sqliteTableExists(t, adapter.db, "usage_summary_by_auth") {
		t.Fatalf("expected usage_summary_by_auth table to exist after InitSchema")
	}
}

func sqliteColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var (
		cid     int
		name    string
		typ     string
		notnull int
		dflt    any
		pk      int
	)
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma rows: %v", err)
	}
	return false
}

func sqliteTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name = ? LIMIT 1", table).Scan(&name)
	if err == nil {
		return name == table
	}
	if err == sql.ErrNoRows {
		return false
	}
	t.Fatalf("sqlite_master query: %v", err)
	return false
}
