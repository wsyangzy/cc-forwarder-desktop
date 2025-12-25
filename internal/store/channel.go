// Package store 提供数据存储层实现
// 渠道存储 - v6.1.0 新增
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// ChannelRecord 表示数据库中的渠道记录
type ChannelRecord struct {
	ID int64 `json:"id"`

	Name     string `json:"name"`
	Website  string `json:"website,omitempty"`
	Priority int    `json:"priority"`
	// FailoverEnabled 表示该渠道是否参与“渠道间故障转移”（暂停/恢复按钮持久化）。
	FailoverEnabled bool `json:"failover_enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChannelStore 定义渠道存储接口
type ChannelStore interface {
	Create(ctx context.Context, record *ChannelRecord) (*ChannelRecord, error)
	Get(ctx context.Context, name string) (*ChannelRecord, error)
	List(ctx context.Context) ([]*ChannelRecord, error)
	Update(ctx context.Context, record *ChannelRecord) error
	Delete(ctx context.Context, name string) error

	WithTx(tx *sql.Tx) ChannelStore
}

// SQLiteChannelStore 实现 ChannelStore 接口
type SQLiteChannelStore struct {
	db *sql.DB
	mu sync.RWMutex
	tx *sql.Tx
}

func NewSQLiteChannelStore(db *sql.DB) *SQLiteChannelStore {
	return &SQLiteChannelStore{db: db}
}

func (s *SQLiteChannelStore) WithTx(tx *sql.Tx) ChannelStore {
	return &SQLiteChannelStore{db: s.db, tx: tx}
}

func (s *SQLiteChannelStore) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if s.tx != nil {
		return s.tx.ExecContext(ctx, query, args...)
	}
	return s.db.ExecContext(ctx, query, args...)
}

func (s *SQLiteChannelStore) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if s.tx != nil {
		return s.tx.QueryRowContext(ctx, query, args...)
	}
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *SQLiteChannelStore) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.tx != nil {
		return s.tx.QueryContext(ctx, query, args...)
	}
	return s.db.QueryContext(ctx, query, args...)
}

func (s *SQLiteChannelStore) Create(ctx context.Context, record *ChannelRecord) (*ChannelRecord, error) {
	if record == nil {
		return nil, fmt.Errorf("record 不能为空")
	}
	if record.Name == "" {
		return nil, fmt.Errorf("渠道名称不能为空")
	}
	if record.Priority <= 0 {
		record.Priority = 1
	}

	s.mu.Lock()
	query := `
		INSERT INTO channels (name, website, priority, failover_enabled)
		VALUES (?, ?, ?, ?)
	`
	res, err := s.execContext(ctx, query, record.Name, nullIfEmpty(record.Website), record.Priority, boolToInt(record.FailoverEnabled))
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("创建渠道失败: %w", err)
	}

	_, _ = res.LastInsertId()
	s.mu.Unlock()

	created, err := s.Get(ctx, record.Name)
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *SQLiteChannelStore) Get(ctx context.Context, name string) (*ChannelRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, name, COALESCE(website, ''), COALESCE(priority, 1), COALESCE(failover_enabled, 1), created_at, updated_at
		FROM channels
		WHERE name = ?
	`

	var record ChannelRecord
	var createdAt, updatedAt string
	var failoverEnabled int

	err := s.queryRowContext(ctx, query, name).Scan(
		&record.ID,
		&record.Name,
		&record.Website,
		&record.Priority,
		&failoverEnabled,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("获取渠道失败: %w", err)
	}

	record.CreatedAt = parseSQLiteDateTime(createdAt)
	record.UpdatedAt = parseSQLiteDateTime(updatedAt)
	record.FailoverEnabled = failoverEnabled != 0

	return &record, nil
}

func (s *SQLiteChannelStore) List(ctx context.Context) ([]*ChannelRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, name, COALESCE(website, ''), COALESCE(priority, 1), COALESCE(failover_enabled, 1), created_at, updated_at
		FROM channels
		ORDER BY COALESCE(priority, 1) ASC, created_at DESC, name ASC
	`
	rows, err := s.queryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("列出渠道失败: %w", err)
	}
	defer rows.Close()

	var result []*ChannelRecord
	for rows.Next() {
		var record ChannelRecord
		var createdAt, updatedAt string
		var failoverEnabled int
		if err := rows.Scan(&record.ID, &record.Name, &record.Website, &record.Priority, &failoverEnabled, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("读取渠道失败: %w", err)
		}
		record.CreatedAt = parseSQLiteDateTime(createdAt)
		record.UpdatedAt = parseSQLiteDateTime(updatedAt)
		record.FailoverEnabled = failoverEnabled != 0
		result = append(result, &record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取渠道失败: %w", err)
	}
	return result, nil
}

func (s *SQLiteChannelStore) Update(ctx context.Context, record *ChannelRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if record == nil {
		return fmt.Errorf("record 不能为空")
	}
	if record.Name == "" {
		return fmt.Errorf("渠道名称不能为空")
	}
	if record.Priority <= 0 {
		record.Priority = 1
	}

	query := `UPDATE channels SET website = ?, priority = ?, failover_enabled = ? WHERE name = ?`
	res, err := s.execContext(ctx, query, nullIfEmpty(record.Website), record.Priority, boolToInt(record.FailoverEnabled), record.Name)
	if err != nil {
		return fmt.Errorf("更新渠道失败: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("渠道不存在: %s", record.Name)
	}
	return nil
}

func (s *SQLiteChannelStore) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.execContext(ctx, `DELETE FROM channels WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("删除渠道失败: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("渠道不存在: %s", name)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
