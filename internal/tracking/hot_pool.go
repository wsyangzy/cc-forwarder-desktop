package tracking

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ActiveRequest 表示一个活跃的请求（完整状态）
// 请求在内存中保持活跃状态，只有完成时才归档到数据库
type ActiveRequest struct {
	// 基础信息（创建时设置，不变）
	RequestID   string    `json:"request_id"`
	StartTime   time.Time `json:"start_time"`
	ClientIP    string    `json:"client_ip"`
	UserAgent   string    `json:"user_agent"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	IsStreaming bool      `json:"is_streaming"`

	// 可变状态（频繁更新）
	Status        string `json:"status"`         // pending/forwarding/processing/completed/failed/cancelled
	Channel       string `json:"channel"`        // 渠道标签（来自端点配置）
	EndpointName  string `json:"endpoint_name"`  // 当前使用的端点
	GroupName     string `json:"group_name"`     // 当前使用的组
	RetryCount    int    `json:"retry_count"`    // 重试次数
	HTTPStatus    int    `json:"http_status"`    // HTTP状态码
	ModelName     string `json:"model_name"`     // 模型名称
	FailureReason string `json:"failure_reason"` // 失败原因
	CancelReason  string `json:"cancel_reason"`  // 取消原因

	// Token 累积（流式请求实时更新）
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	CacheCreationTokens   int64 `json:"cache_creation_tokens"`    // 总缓存创建（向后兼容）
	CacheCreation5mTokens int64 `json:"cache_creation_5m_tokens"` // 5分钟缓存 (v5.0.1+)
	CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens"` // 1小时缓存 (v5.0.1+)
	CacheReadTokens       int64 `json:"cache_read_tokens"`

	// 完成信息（只在结束时填充）
	EndTime      *time.Time `json:"end_time,omitempty"`
	DurationMs   int64      `json:"duration_ms"`
	TotalCostUSD float64    `json:"total_cost_usd"`

	// 内部字段
	mu sync.RWMutex `json:"-"` // 保护单个请求的并发访问
}

// HotPoolConfig 热池配置
type HotPoolConfig struct {
	MaxAge           time.Duration // 最大存活时间，防止泄漏（默认30分钟）
	MaxSize          int           // 最大容量（默认10000）
	CleanupInterval  time.Duration // 清理间隔（默认1分钟）
	ArchiveOnCleanup bool          // 清理时是否归档（默认true）
}

// DefaultHotPoolConfig 返回默认配置
func DefaultHotPoolConfig() HotPoolConfig {
	return HotPoolConfig{
		MaxAge:           30 * time.Minute,
		MaxSize:          10000,
		CleanupInterval:  1 * time.Minute,
		ArchiveOnCleanup: true,
	}
}

// HotPool 管理所有活跃请求的内存状态
type HotPool struct {
	mu       sync.RWMutex
	requests map[string]*ActiveRequest

	// 归档中的请求（已完成但还没写入数据库）
	// 这个缓存确保请求在"热池→数据库"过渡期间不会丢失
	archiving map[string]*ActiveRequest

	config HotPoolConfig

	// 归档回调（当请求需要归档时调用）
	archiveCallback func(*ActiveRequest)

	// 统计信息
	stats HotPoolStats

	// 生命周期控制
	ctx    chan struct{}
	closed bool
}

// HotPoolStats 热池统计信息
type HotPoolStats struct {
	mu              sync.RWMutex
	TotalAdded      int64 `json:"total_added"`       // 累计添加次数
	TotalRemoved    int64 `json:"total_removed"`     // 累计移除次数
	TotalArchived   int64 `json:"total_archived"`    // 累计归档次数
	TotalExpired    int64 `json:"total_expired"`     // 累计过期清理次数
	TotalOverflow   int64 `json:"total_overflow"`    // 累计溢出丢弃次数
	CurrentSize     int   `json:"current_size"`      // 当前大小
	PeakSize        int   `json:"peak_size"`         // 峰值大小
}

// NewHotPool 创建热池
func NewHotPool(config HotPoolConfig) *HotPool {
	if config.MaxAge <= 0 {
		config.MaxAge = 30 * time.Minute
	}
	if config.MaxSize <= 0 {
		config.MaxSize = 10000
	}
	if config.CleanupInterval <= 0 {
		config.CleanupInterval = 1 * time.Minute
	}

	pool := &HotPool{
		requests:  make(map[string]*ActiveRequest),
		archiving: make(map[string]*ActiveRequest),
		config:    config,
		ctx:       make(chan struct{}),
	}

	// 启动清理协程
	go pool.startCleaner()

	slog.Info("🔥 HotPool 初始化完成",
		"max_age", config.MaxAge,
		"max_size", config.MaxSize,
		"cleanup_interval", config.CleanupInterval)

	return pool
}

// SetArchiveCallback 设置归档回调函数
func (hp *HotPool) SetArchiveCallback(callback func(*ActiveRequest)) {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	hp.archiveCallback = callback
}

// Add 添加新请求到热池
func (hp *HotPool) Add(req *ActiveRequest) error {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	if hp.closed {
		return fmt.Errorf("hot pool is closed")
	}

	// 检查是否已存在
	if _, exists := hp.requests[req.RequestID]; exists {
		return fmt.Errorf("request %s already exists in hot pool", req.RequestID)
	}

	// 检查容量
	if len(hp.requests) >= hp.config.MaxSize {
		hp.stats.mu.Lock()
		hp.stats.TotalOverflow++
		hp.stats.mu.Unlock()
		slog.Warn("🔥 HotPool 容量已满，拒绝新请求",
			"request_id", req.RequestID,
			"current_size", len(hp.requests),
			"max_size", hp.config.MaxSize)
		return fmt.Errorf("hot pool is full (max: %d)", hp.config.MaxSize)
	}

	hp.requests[req.RequestID] = req

	// 更新统计
	hp.stats.mu.Lock()
	hp.stats.TotalAdded++
	hp.stats.CurrentSize = len(hp.requests)
	if hp.stats.CurrentSize > hp.stats.PeakSize {
		hp.stats.PeakSize = hp.stats.CurrentSize
	}
	hp.stats.mu.Unlock()

	return nil
}

// Update 更新请求状态（高频操作，纯内存）
func (hp *HotPool) Update(requestID string, updater func(*ActiveRequest)) error {
	hp.mu.RLock()
	req, exists := hp.requests[requestID]
	hp.mu.RUnlock()

	if !exists {
		return fmt.Errorf("request %s not found in hot pool", requestID)
	}

	// 使用请求级别的锁进行更新
	req.mu.Lock()
	updater(req)
	req.mu.Unlock()

	return nil
}

// Get 获取请求（只读）
func (hp *HotPool) Get(requestID string) (*ActiveRequest, bool) {
	hp.mu.RLock()
	defer hp.mu.RUnlock()

	req, exists := hp.requests[requestID]
	return req, exists
}

// Remove 从热池中移除请求（归档时调用）
func (hp *HotPool) Remove(requestID string) *ActiveRequest {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	req, exists := hp.requests[requestID]
	if !exists {
		return nil
	}

	delete(hp.requests, requestID)

	// 更新统计
	hp.stats.mu.Lock()
	hp.stats.TotalRemoved++
	hp.stats.CurrentSize = len(hp.requests)
	hp.stats.mu.Unlock()

	return req
}

// Archive 归档请求（移除并发送到归档通道）
func (hp *HotPool) Archive(requestID string) error {
	req := hp.Remove(requestID)
	if req == nil {
		return fmt.Errorf("request %s not found in hot pool", requestID)
	}

	// 更新统计
	hp.stats.mu.Lock()
	hp.stats.TotalArchived++
	hp.stats.mu.Unlock()

	// 调用归档回调
	hp.mu.RLock()
	callback := hp.archiveCallback
	hp.mu.RUnlock()

	if callback != nil {
		callback(req)
	}

	return nil
}

// CompleteAndArchive 完成请求并归档（一步完成）
// 请求会先移到 archiving 缓存，确保在写入数据库前不会丢失
func (hp *HotPool) CompleteAndArchive(requestID string, finalizer func(*ActiveRequest)) error {
	hp.mu.Lock()
	req, exists := hp.requests[requestID]
	if !exists {
		hp.mu.Unlock()
		return fmt.Errorf("request %s not found in hot pool", requestID)
	}
	// 从活跃列表移到归档中列表（而不是直接删除）
	delete(hp.requests, requestID)
	hp.archiving[requestID] = req
	callback := hp.archiveCallback
	hp.mu.Unlock()

	// 应用最终更新
	req.mu.Lock()
	if finalizer != nil {
		finalizer(req)
	}
	// 确保结束时间已设置
	if req.EndTime == nil {
		now := time.Now()
		req.EndTime = &now
		req.DurationMs = now.Sub(req.StartTime).Milliseconds()
	}
	req.mu.Unlock()

	// 更新统计
	hp.stats.mu.Lock()
	hp.stats.TotalRemoved++
	hp.stats.TotalArchived++
	hp.stats.CurrentSize = len(hp.requests)
	hp.stats.mu.Unlock()

	// 调用归档回调
	if callback != nil {
		callback(req)
	}

	return nil
}

// GetActive 获取所有活跃请求（用于 Web 界面）
// 包括正在处理的请求 + 正在归档中的请求
func (hp *HotPool) GetActive() []*ActiveRequest {
	hp.mu.RLock()
	defer hp.mu.RUnlock()

	// 容量 = 活跃请求 + 归档中请求
	result := make([]*ActiveRequest, 0, len(hp.requests)+len(hp.archiving))

	// 添加活跃请求
	for _, req := range hp.requests {
		result = append(result, req)
	}

	// 添加归档中的请求（确保过渡期不丢失）
	for _, req := range hp.archiving {
		result = append(result, req)
	}

	return result
}

// GetActiveCount 获取活跃请求数量（包括归档中的）
func (hp *HotPool) GetActiveCount() int {
	hp.mu.RLock()
	defer hp.mu.RUnlock()
	return len(hp.requests) + len(hp.archiving)
}

// ConfirmArchived 确认请求已成功写入数据库，从归档缓存中移除
// 由 ArchiveManager 在批量写入成功后调用
func (hp *HotPool) ConfirmArchived(requestIDs []string) {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	for _, id := range requestIDs {
		delete(hp.archiving, id)
	}
}

// GetArchivingCount 获取正在归档中的请求数量（调试用）
func (hp *HotPool) GetArchivingCount() int {
	hp.mu.RLock()
	defer hp.mu.RUnlock()
	return len(hp.archiving)
}

// GetStats 获取统计信息
func (hp *HotPool) GetStats() HotPoolStats {
	hp.stats.mu.RLock()
	defer hp.stats.mu.RUnlock()

	return HotPoolStats{
		TotalAdded:    hp.stats.TotalAdded,
		TotalRemoved:  hp.stats.TotalRemoved,
		TotalArchived: hp.stats.TotalArchived,
		TotalExpired:  hp.stats.TotalExpired,
		TotalOverflow: hp.stats.TotalOverflow,
		CurrentSize:   hp.stats.CurrentSize,
		PeakSize:      hp.stats.PeakSize,
	}
}

// Exists 检查请求是否存在于热池中
func (hp *HotPool) Exists(requestID string) bool {
	hp.mu.RLock()
	defer hp.mu.RUnlock()
	_, exists := hp.requests[requestID]
	return exists
}

// startCleaner 定期清理僵尸请求
func (hp *HotPool) startCleaner() {
	ticker := time.NewTicker(hp.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			hp.cleanup()
		case <-hp.ctx:
			return
		}
	}
}

// cleanup 清理过期请求
func (hp *HotPool) cleanup() {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	now := time.Now()
	expiredRequests := make([]*ActiveRequest, 0)

	for id, req := range hp.requests {
		if now.Sub(req.StartTime) > hp.config.MaxAge {
			// 标记为超时
			req.mu.Lock()
			req.Status = "timeout"
			req.FailureReason = "hot pool cleanup: request exceeded max age"
			req.EndTime = &now
			req.DurationMs = now.Sub(req.StartTime).Milliseconds()
			req.mu.Unlock()

			expiredRequests = append(expiredRequests, req)
			delete(hp.requests, id)
		}
	}

	if len(expiredRequests) > 0 {
		// 更新统计
		hp.stats.mu.Lock()
		hp.stats.TotalExpired += int64(len(expiredRequests))
		hp.stats.TotalRemoved += int64(len(expiredRequests))
		hp.stats.CurrentSize = len(hp.requests)
		hp.stats.mu.Unlock()

		slog.Warn("🔥 HotPool 清理过期请求",
			"expired_count", len(expiredRequests),
			"remaining", len(hp.requests))

		// 归档过期请求（如果启用）
		if hp.config.ArchiveOnCleanup && hp.archiveCallback != nil {
			for _, req := range expiredRequests {
				hp.archiveCallback(req)
				hp.stats.mu.Lock()
				hp.stats.TotalArchived++
				hp.stats.mu.Unlock()
			}
		}
	}
}

// Close 关闭热池
func (hp *HotPool) Close() error {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	if hp.closed {
		return nil
	}

	hp.closed = true
	close(hp.ctx)

	// 归档所有剩余请求
	now := time.Now()
	for _, req := range hp.requests {
		req.mu.Lock()
		if req.EndTime == nil {
			req.EndTime = &now
			req.DurationMs = now.Sub(req.StartTime).Milliseconds()
		}
		if req.Status == "pending" || req.Status == "forwarding" || req.Status == "processing" {
			req.Status = "cancelled"
			req.CancelReason = "hot pool shutdown"
		}
		req.mu.Unlock()

		if hp.archiveCallback != nil {
			hp.archiveCallback(req)
		}
	}

	remaining := len(hp.requests)
	hp.requests = make(map[string]*ActiveRequest)

	slog.Info("🔥 HotPool 已关闭",
		"archived_remaining", remaining,
		"total_added", hp.stats.TotalAdded,
		"total_archived", hp.stats.TotalArchived)

	return nil
}

// NewActiveRequest 创建新的活跃请求
func NewActiveRequest(requestID, clientIP, userAgent, method, path string, isStreaming bool) *ActiveRequest {
	return &ActiveRequest{
		RequestID:   requestID,
		StartTime:   time.Now(),
		ClientIP:    clientIP,
		UserAgent:   userAgent,
		Method:      method,
		Path:        path,
		IsStreaming: isStreaming,
		Status:      "pending",
	}
}
