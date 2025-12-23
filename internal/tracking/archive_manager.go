package tracking

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ArchiveEvent 归档事件（请求完成时发送）
type ArchiveEvent struct {
	Request   *ActiveRequest
	Timestamp time.Time
}

// ArchiveManagerConfig 归档管理器配置
type ArchiveManagerConfig struct {
	ChannelSize   int           // 归档通道大小（默认1000）
	BatchSize     int           // 批次大小（默认100）
	FlushInterval time.Duration // 刷新间隔（默认5秒）
	MaxRetry      int           // 最大重试次数（默认3）
}

// DefaultArchiveManagerConfig 返回默认配置
func DefaultArchiveManagerConfig() ArchiveManagerConfig {
	return ArchiveManagerConfig{
		ChannelSize:   1000,
		BatchSize:     100,
		FlushInterval: 5 * time.Second,
		MaxRetry:      3,
	}
}

// ArchiveManager 管理归档写入
type ArchiveManager struct {
	archiveChan chan *ArchiveEvent
	adapter     DatabaseAdapter
	config      ArchiveManagerConfig
	pricing     map[string]ModelPricing       // 模型定价缓存
	endpointMu  map[string]EndpointMultiplier // 端点倍率缓存
	location    *time.Location

	// 热池引用（用于归档成功后清理）
	hotPool *HotPool

	// 统计信息
	stats ArchiveStats

	// 生命周期控制
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ArchiveStats 归档统计信息
type ArchiveStats struct {
	mu               sync.RWMutex
	TotalReceived    int64         `json:"total_received"`     // 累计接收事件数
	TotalArchived    int64         `json:"total_archived"`     // 累计归档成功数
	TotalFailed      int64         `json:"total_failed"`       // 累计归档失败数
	TotalDropped     int64         `json:"total_dropped"`      // 累计丢弃数（通道满）
	TotalBatches     int64         `json:"total_batches"`      // 累计批次数
	AvgBatchSize     float64       `json:"avg_batch_size"`     // 平均批次大小
	LastFlushTime    time.Time     `json:"last_flush_time"`    // 最后刷新时间
	LastFlushLatency time.Duration `json:"last_flush_latency"` // 最后刷新延迟
	ChannelLength    int           `json:"channel_length"`     // 当前通道长度
}

// NewArchiveManager 创建归档管理器
func NewArchiveManager(adapter DatabaseAdapter, config ArchiveManagerConfig, pricing map[string]ModelPricing, location *time.Location) *ArchiveManager {
	if config.ChannelSize <= 0 {
		config.ChannelSize = 1000
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = 5 * time.Second
	}
	if config.MaxRetry <= 0 {
		config.MaxRetry = 3
	}

	ctx, cancel := context.WithCancel(context.Background())

	am := &ArchiveManager{
		archiveChan: make(chan *ArchiveEvent, config.ChannelSize),
		adapter:     adapter,
		config:      config,
		pricing:     pricing,
		location:    location,
		ctx:         ctx,
		cancel:      cancel,
	}

	// 启动归档协程
	am.wg.Add(1)
	go am.startArchiver()

	slog.Info("📦 ArchiveManager 初始化完成",
		"channel_size", config.ChannelSize,
		"batch_size", config.BatchSize,
		"flush_interval", config.FlushInterval)

	return am
}

// SetHotPool 设置热池引用（用于归档成功后清理）
func (am *ArchiveManager) SetHotPool(hp *HotPool) {
	am.hotPool = hp
}

// UpdateEndpointMultipliers 更新端点成本倍率
func (am *ArchiveManager) UpdateEndpointMultipliers(multipliers map[string]EndpointMultiplier) {
	am.endpointMu = multipliers
}

// UpdatePricing 更新模型定价（运行时动态更新）
func (am *ArchiveManager) UpdatePricing(pricing map[string]ModelPricing) {
	am.pricing = pricing
}

// Archive 发送请求到归档通道
func (am *ArchiveManager) Archive(req *ActiveRequest) error {
	event := &ArchiveEvent{
		Request:   req,
		Timestamp: time.Now(),
	}

	// 非阻塞发送
	select {
	case am.archiveChan <- event:
		am.stats.mu.Lock()
		am.stats.TotalReceived++
		am.stats.mu.Unlock()
		return nil
	default:
		am.stats.mu.Lock()
		am.stats.TotalDropped++
		am.stats.mu.Unlock()
		slog.Warn("📦 归档通道已满，丢弃事件",
			"request_id", req.RequestID,
			"channel_length", len(am.archiveChan))
		return fmt.Errorf("archive channel full, event dropped")
	}
}

// startArchiver 批量归档协程
func (am *ArchiveManager) startArchiver() {
	defer am.wg.Done()

	batch := make([]*ArchiveEvent, 0, am.config.BatchSize)
	ticker := time.NewTicker(am.config.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}

		// 提取请求 ID 列表
		requestIDs := make([]string, len(batch))
		for i, event := range batch {
			requestIDs[i] = event.Request.RequestID
		}

		start := time.Now()
		err := am.flushBatch(batch)
		latency := time.Since(start)

		am.stats.mu.Lock()
		am.stats.LastFlushTime = time.Now()
		am.stats.LastFlushLatency = latency
		am.stats.TotalBatches++
		if err == nil {
			am.stats.TotalArchived += int64(len(batch))
			// 更新平均批次大小
			am.stats.AvgBatchSize = float64(am.stats.TotalArchived) / float64(am.stats.TotalBatches)
		} else {
			am.stats.TotalFailed += int64(len(batch))
		}
		am.stats.mu.Unlock()

		if err != nil {
			slog.Error("📦 批量归档失败",
				"batch_size", len(batch),
				"request_ids", strings.Join(requestIDs, ", "),
				"error", err,
				"latency", latency)
		} else {
			slog.Debug("📦 批量归档成功",
				"batch_size", len(batch),
				"request_ids", strings.Join(requestIDs, ", "),
				"latency", latency)
		}

		// 清空批次
		batch = batch[:0]
	}

	for {
		select {
		case event := <-am.archiveChan:
			batch = append(batch, event)

			// 达到批次大小，立即刷新
			if len(batch) >= am.config.BatchSize {
				flush()
			}

		case <-ticker.C:
			// 定时刷新（即使未满批次）
			flush()

		case <-am.ctx.Done():
			// 优雅关闭，处理剩余事件
			slog.Info("📦 ArchiveManager 正在关闭，处理剩余事件...")

			// 先刷新当前批次
			flush()

			// 处理通道中剩余的事件
			for {
				select {
				case event := <-am.archiveChan:
					batch = append(batch, event)
					if len(batch) >= am.config.BatchSize {
						flush()
					}
				default:
					// 通道已空，最后刷新
					flush()
					slog.Info("📦 ArchiveManager 已关闭",
						"total_archived", am.stats.TotalArchived,
						"total_failed", am.stats.TotalFailed)
					return
				}
			}
		}
	}
}

// flushBatch 批量写入数据库
func (am *ArchiveManager) flushBatch(events []*ArchiveEvent) error {
	if len(events) == 0 {
		return nil
	}

	var lastErr error
	for retry := 0; retry < am.config.MaxRetry; retry++ {
		err := am.batchInsert(events)
		if err == nil {
			// 归档成功，从热池的归档缓存中移除
			if am.hotPool != nil {
				requestIDs := make([]string, len(events))
				for i, event := range events {
					requestIDs[i] = event.Request.RequestID
				}
				am.hotPool.ConfirmArchived(requestIDs)
			}
			return nil
		}

		lastErr = err
		slog.Warn("📦 批量插入失败，重试中",
			"retry", retry+1,
			"max_retry", am.config.MaxRetry,
			"error", err)

		// 指数退避
		time.Sleep(time.Duration(retry+1) * time.Second)
	}

	return fmt.Errorf("batch insert failed after %d retries: %w", am.config.MaxRetry, lastErr)
}

// batchInsert 执行批量插入
func (am *ArchiveManager) batchInsert(events []*ArchiveEvent) error {
	if am.adapter == nil {
		return fmt.Errorf("database adapter not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := am.adapter.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// v5.0.1+: 添加分开的 5m/1h 缓存字段和成本
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO request_logs (
			request_id, client_ip, user_agent, method, path,
			start_time, end_time, duration_ms,
			channel, endpoint_name, group_name, model_name,
			status, http_status_code, retry_count,
			failure_reason, cancel_reason,
			is_streaming,
			input_tokens, output_tokens,
			cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens,
			cache_read_tokens,
			input_cost_usd, output_cost_usd,
			cache_creation_cost_usd, cache_creation_5m_cost_usd, cache_creation_1h_cost_usd,
			cache_read_cost_usd, total_cost_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, event := range events {
		req := event.Request

		// v5.0.1+: 计算分开的成本
		costBreakdown := am.calculateCostV2(req)

		// 格式化时间
		startTime := am.formatTime(req.StartTime)
		var endTime interface{}
		if req.EndTime != nil {
			endTime = am.formatTime(*req.EndTime)
		} else {
			endTime = nil
		}

		_, err := stmt.ExecContext(ctx,
			req.RequestID,
			req.ClientIP,
			req.UserAgent,
			req.Method,
			req.Path,
			startTime,
			endTime,
			req.DurationMs,
			req.Channel,
			req.EndpointName,
			req.GroupName,
			req.ModelName,
			req.Status,
			req.HTTPStatus,
			req.RetryCount,
			nullString(req.FailureReason),
			nullString(req.CancelReason),
			req.IsStreaming,
			req.InputTokens,
			req.OutputTokens,
			req.CacheCreationTokens,
			req.CacheCreation5mTokens,
			req.CacheCreation1hTokens,
			req.CacheReadTokens,
			costBreakdown.InputCost,
			costBreakdown.OutputCost,
			costBreakdown.CacheCreationCost,   // 总成本（向后兼容）
			costBreakdown.CacheCreation5mCost, // 5分钟缓存成本
			costBreakdown.CacheCreation1hCost, // 1小时缓存成本
			costBreakdown.CacheReadCost,
			costBreakdown.TotalCost,
		)
		if err != nil {
			return fmt.Errorf("failed to insert request %s: %w", req.RequestID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// calculateCostV2 计算请求成本（v5.0.1+: 支持分开的 5m/1h 缓存）
func (am *ArchiveManager) calculateCostV2(req *ActiveRequest) CostBreakdown {
	if am.pricing == nil {
		return CostBreakdown{}
	}

	// 查找模型定价，不存在则回退到 _default 定价
	pricing, exists := am.pricing[req.ModelName]
	if !exists {
		pricing, exists = am.pricing["_default"]
		if !exists {
			return CostBreakdown{}
		}
	}

	// 获取端点倍率
	var multiplier *EndpointMultiplier
	if am.endpointMu != nil {
		if m, ok := am.endpointMu[req.EndpointName]; ok {
			multiplier = &m
		}
	}

	// 构建 TokenUsage 对象
	usage := &TokenUsage{
		InputTokens:           req.InputTokens,
		OutputTokens:          req.OutputTokens,
		CacheCreationTokens:   req.CacheCreationTokens,
		CacheCreation5mTokens: req.CacheCreation5mTokens,
		CacheCreation1hTokens: req.CacheCreation1hTokens,
		CacheReadTokens:       req.CacheReadTokens,
	}

	// 调用公共成本计算函数（v5.0.1+）
	return CalculateCostV2(usage, &pricing, multiplier)
}

// formatTime 格式化时间为易读格式（使用配置的时区）
// 格式：2025-12-04 17:18:48（北京时间，无时区后缀）
func (am *ArchiveManager) formatTime(t time.Time) string {
	if am.location != nil {
		t = t.In(am.location)
	}
	return t.Format("2006-01-02 15:04:05")
}

// nullString 处理空字符串为SQL NULL
func nullString(s string) interface{} {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

// GetStats 获取统计信息（返回快照，不含锁）
func (am *ArchiveManager) GetStats() ArchiveStats {
	am.stats.mu.RLock()
	defer am.stats.mu.RUnlock()

	// 返回不含锁的快照副本
	return ArchiveStats{
		TotalReceived:    am.stats.TotalReceived,
		TotalArchived:    am.stats.TotalArchived,
		TotalFailed:      am.stats.TotalFailed,
		TotalDropped:     am.stats.TotalDropped,
		TotalBatches:     am.stats.TotalBatches,
		AvgBatchSize:     am.stats.AvgBatchSize,
		LastFlushTime:    am.stats.LastFlushTime,
		LastFlushLatency: am.stats.LastFlushLatency,
		ChannelLength:    len(am.archiveChan),
		// 注意：mu 字段是零值，不会复制原始锁
	}
}

// GetChannelLength 获取当前通道长度
func (am *ArchiveManager) GetChannelLength() int {
	return len(am.archiveChan)
}

// Close 关闭归档管理器
func (am *ArchiveManager) Close() error {
	slog.Info("📦 正在关闭 ArchiveManager...")
	am.cancel()
	am.wg.Wait()
	return nil
}

// Flush 强制刷新（用于测试或优雅关闭）
func (am *ArchiveManager) Flush() {
	// 发送一个空事件触发刷新
	// 实际刷新由 ticker 处理，这里只是等待
	time.Sleep(100 * time.Millisecond)
}
