package tracking

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 回归测试：旧库缺少后续新增列时，InitSchema 应能通过 migrateSchema 补齐关键列，避免启动失败。
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

	// 1) 造一个“旧版本”数据库：request_logs/endpoints/channels 存在，但缺少后续新增列（5m/1h 缓存字段、端点超时、渠道优先级等）。
	// 注意：旧表存在时，schema.sql 的 CREATE TABLE IF NOT EXISTS 不会补列，需要 migrateSchema 补齐。
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

	// 3) 验证关键列已存在（由 migrateSchema 补齐）
	for _, c := range []string{"cache_creation_5m_tokens", "cache_creation_1h_tokens", "cache_creation_5m_cost_usd", "cache_creation_1h_cost_usd"} {
		if !sqliteColumnExists(t, adapter.db, "request_logs", c) {
			t.Fatalf("expected request_logs.%s to exist after InitSchema", c)
		}
	}
	for _, c := range []string{"timeout_seconds", "supports_count_tokens"} {
		if !sqliteColumnExists(t, adapter.db, "endpoints", c) {
			t.Fatalf("expected endpoints.%s to exist after InitSchema", c)
		}
	}
	if !sqliteColumnExists(t, adapter.db, "channels", "priority") {
		t.Fatalf("expected channels.priority to exist after InitSchema")
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
