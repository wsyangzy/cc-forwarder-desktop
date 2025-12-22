package tracking

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"cc-forwarder/config"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// UsageStatsDetailed 详细的使用统计
type UsageStatsDetailed struct {
	TotalRequests   int64                   `json:"total_requests"`
	SuccessRequests int64                   `json:"success_requests"`
	ErrorRequests   int64                   `json:"error_requests"`
	TotalTokens     int64                   `json:"total_tokens"`
	TotalCost       float64                 `json:"total_cost"`
	ModelStats      map[string]ModelStat    `json:"model_stats"`
	EndpointStats   map[string]EndpointStat `json:"endpoint_stats"`
	GroupStats      map[string]GroupStat    `json:"group_stats"`
}

// ModelStat 模型统计
type ModelStat struct {
	RequestCount int64   `json:"request_count"`
	TotalCost    float64 `json:"total_cost"`
}

// EndpointStat 端点统计
type EndpointStat struct {
	RequestCount int64   `json:"request_count"`
	TotalCost    float64 `json:"total_cost"`
}

// GroupStat 组统计
type GroupStat struct {
	RequestCount int64   `json:"request_count"`
	TotalCost    float64 `json:"total_cost"`
}

// RequestEvent 表示请求事件
type RequestEvent struct {
	Type      string      `json:"type"` // "start", "flexible_update", "success", "final_failure", "complete", "failed_request_tokens", "token_recovery"
	RequestID string      `json:"request_id"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"` // 根据Type不同而变化
}

// RequestStartData 请求开始事件数据
type RequestStartData struct {
	ClientIP    string `json:"client_ip"`
	UserAgent   string `json:"user_agent"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	IsStreaming bool   `json:"is_streaming"` // 是否为流式请求
}

// RequestUpdateData 请求更新事件数据
type RequestUpdateData struct {
	Channel      string `json:"channel"`
	EndpointName string `json:"endpoint_name"`
	GroupName    string `json:"group_name"`
	Status       string `json:"status"`
	RetryCount   int    `json:"retry_count"`
	HTTPStatus   int    `json:"http_status"`
	// 🚀 [状态机重构] Phase 2: 新增失败原因和取消原因字段
	FailureReason string `json:"failure_reason,omitempty"`
	CancelReason  string `json:"cancel_reason,omitempty"`
}

// RequestCompleteData 请求完成事件数据
type RequestCompleteData struct {
	ModelName             string        `json:"model_name"`
	InputTokens           int64         `json:"input_tokens"`
	OutputTokens          int64         `json:"output_tokens"`
	CacheCreationTokens   int64         `json:"cache_creation_tokens"`
	CacheCreation5mTokens int64         `json:"cache_creation_5m_tokens"` // v5.0.1+
	CacheCreation1hTokens int64         `json:"cache_creation_1h_tokens"` // v5.0.1+
	CacheReadTokens       int64         `json:"cache_read_tokens"`
	Duration              time.Duration `json:"duration"`
	FailureReason         string        `json:"failure_reason,omitempty"` // 可选：失败原因
}

// TokenUsage token使用统计
type TokenUsage struct {
	InputTokens           int64
	OutputTokens          int64
	CacheCreationTokens   int64 // 总缓存创建 tokens（向后兼容，= 5m + 1h）
	CacheReadTokens       int64
	CacheCreation5mTokens int64 // 5分钟缓存创建 tokens (1.25x 定价)
	CacheCreation1hTokens int64 // 1小时缓存创建 tokens (2x 定价)
}

// ModelPricing 模型定价配置
type ModelPricing struct {
	Input           float64 `yaml:"input"`             // per 1M tokens
	Output          float64 `yaml:"output"`            // per 1M tokens
	CacheCreation   float64 `yaml:"cache_creation"`    // per 1M tokens (5分钟缓存创建，1.25x input)
	CacheCreation1h float64 `yaml:"cache_creation_1h"` // per 1M tokens (1小时缓存创建，2x input)
	CacheRead       float64 `yaml:"cache_read"`        // per 1M tokens (缓存读取)
}

// EndpointMultiplier 端点成本倍率（v5.0+ 支持端点级别的成本调整）
// 成本计算公式：模型基础定价 * 端点倍率
type EndpointMultiplier struct {
	CostMultiplier                float64 // 总体倍率（如果 > 0，会覆盖分项倍率）
	InputCostMultiplier           float64 // 输入成本倍率
	OutputCostMultiplier          float64 // 输出成本倍率
	CacheCreationCostMultiplier   float64 // 5分钟缓存创建成本倍率
	CacheCreationCostMultiplier1h float64 // 1小时缓存创建成本倍率
	CacheReadCostMultiplier       float64 // 缓存读取成本倍率
}

// DefaultEndpointMultiplier 返回默认端点倍率（全部为 1.0）
func DefaultEndpointMultiplier() EndpointMultiplier {
	return EndpointMultiplier{
		CostMultiplier:                1.0,
		InputCostMultiplier:           1.0,
		OutputCostMultiplier:          1.0,
		CacheCreationCostMultiplier:   1.0,
		CacheCreationCostMultiplier1h: 1.0,
		CacheReadCostMultiplier:       1.0,
	}
}

// CostBreakdown 成本分解结构
type CostBreakdown struct {
	InputCost           float64
	OutputCost          float64
	CacheCreationCost   float64 // 总缓存创建成本 (5m + 1h)
	CacheCreation5mCost float64 // 5分钟缓存创建成本
	CacheCreation1hCost float64 // 1小时缓存创建成本
	CacheReadCost       float64
	TotalCost           float64
}

// CalculateCostV2 统一的成本计算函数（v5.0+ 支持分开的缓存定价）
// 消除 archive_manager.go 和 tracker.go 中的重复代码
//
// 参数:
//   - usage: Token 使用量（包含分开的 5m/1h 缓存 tokens）
//   - pricing: 模型定价，nil 时返回零成本
//   - multiplier: 端点倍率，nil 时使用默认倍率 1.0
func CalculateCostV2(usage *TokenUsage, pricing *ModelPricing, multiplier *EndpointMultiplier) CostBreakdown {
	if pricing == nil || usage == nil {
		return CostBreakdown{}
	}

	// 使用默认倍率
	var m EndpointMultiplier
	if multiplier != nil {
		m = *multiplier
	} else {
		m = DefaultEndpointMultiplier()
	}

	// 确保倍率有效（0 或负数视为 1.0）
	ensureMultiplier := func(v float64) float64 {
		if v <= 0 {
			return 1.0
		}
		return v
	}

	// 获取 1h 缓存定价（如果未设置，使用 2x input 作为默认）
	cacheCreation1hPrice := pricing.CacheCreation1h
	if cacheCreation1hPrice <= 0 {
		cacheCreation1hPrice = pricing.Input * 2.0 // 默认 2x input
	}

	// 基础成本计算（每百万 token）
	inputCost := float64(usage.InputTokens) * pricing.Input / 1_000_000
	outputCost := float64(usage.OutputTokens) * pricing.Output / 1_000_000
	cacheReadCost := float64(usage.CacheReadTokens) * pricing.CacheRead / 1_000_000

	// 分开计算 5m 和 1h 缓存成本
	var cache5mCost, cache1hCost float64
	if usage.CacheCreation5mTokens > 0 || usage.CacheCreation1hTokens > 0 {
		// 有分开的缓存数据，分别计算
		cache5mCost = float64(usage.CacheCreation5mTokens) * pricing.CacheCreation / 1_000_000
		cache1hCost = float64(usage.CacheCreation1hTokens) * cacheCreation1hPrice / 1_000_000
	} else {
		// 向后兼容：只有总数，默认使用 5m 定价
		cache5mCost = float64(usage.CacheCreationTokens) * pricing.CacheCreation / 1_000_000
		cache1hCost = 0
	}
	cacheCreationCost := cache5mCost + cache1hCost

	// 应用端点倍率
	if m.CostMultiplier > 0 {
		// 总体倍率模式：所有成本统一乘以总体倍率
		totalMultiplier := ensureMultiplier(m.CostMultiplier)
		inputCost *= totalMultiplier
		outputCost *= totalMultiplier
		cache5mCost *= totalMultiplier
		cache1hCost *= totalMultiplier
		cacheCreationCost *= totalMultiplier
		cacheReadCost *= totalMultiplier
	} else {
		// 分项倍率模式：各项成本分别乘以对应倍率
		inputCost *= ensureMultiplier(m.InputCostMultiplier)
		outputCost *= ensureMultiplier(m.OutputCostMultiplier)
		cacheReadCost *= ensureMultiplier(m.CacheReadCostMultiplier)
		cache5mCost *= ensureMultiplier(m.CacheCreationCostMultiplier)
		cache1hCost *= ensureMultiplier(m.CacheCreationCostMultiplier1h)
		cacheCreationCost = cache5mCost + cache1hCost
	}

	return CostBreakdown{
		InputCost:           inputCost,
		OutputCost:          outputCost,
		CacheCreationCost:   cacheCreationCost,
		CacheCreation5mCost: cache5mCost,
		CacheCreation1hCost: cache1hCost,
		CacheReadCost:       cacheReadCost,
		TotalCost:           inputCost + outputCost + cacheCreationCost + cacheReadCost,
	}
}

// CalculateCost 统一的成本计算函数（向后兼容）
// Deprecated: 使用 CalculateCostV2 以支持分开的缓存定价
//
// 参数:
//   - inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens: Token 使用量
//   - pricing: 模型定价，nil 时返回零成本
//   - multiplier: 端点倍率，nil 时使用默认倍率 1.0
//   - use1hCache: 是否使用 1 小时缓存倍率（用于长效缓存场景）
func CalculateCost(inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64,
	pricing *ModelPricing, multiplier *EndpointMultiplier, use1hCache bool) CostBreakdown {

	// 构建 TokenUsage，根据 use1hCache 决定放入哪个字段
	usage := &TokenUsage{
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		CacheReadTokens: cacheReadTokens,
	}
	if use1hCache {
		usage.CacheCreation1hTokens = cacheCreationTokens
	} else {
		usage.CacheCreation5mTokens = cacheCreationTokens
	}
	usage.CacheCreationTokens = cacheCreationTokens // 保持总数

	return CalculateCostV2(usage, pricing, multiplier)
}

// Config 使用跟踪配置
type Config struct {
	Enabled bool `yaml:"enabled"`

	// 向后兼容：保留原有的 database_path 配置
	DatabasePath string `yaml:"database_path"`

	// 新增：数据库配置（优先级高于 DatabasePath）
	Database *config.DatabaseBackendConfig `yaml:"database,omitempty"`

	BufferSize      int                     `yaml:"buffer_size"`
	BatchSize       int                     `yaml:"batch_size"`
	FlushInterval   time.Duration           `yaml:"flush_interval"`
	MaxRetry        int                     `yaml:"max_retry"`
	RetentionDays   int                     `yaml:"retention_days"`
	CleanupInterval time.Duration           `yaml:"cleanup_interval"`
	ModelPricing    map[string]ModelPricing `yaml:"model_pricing"`
	DefaultPricing  ModelPricing            `yaml:"default_pricing"`

	// 🔥 v4.1 新增：热池配置
	HotPool *HotPoolSettings `yaml:"hot_pool,omitempty"`
}

// HotPoolSettings 热池配置
type HotPoolSettings struct {
	Enabled          bool          `yaml:"enabled"`            // 是否启用热池模式（默认true）
	MaxAge           time.Duration `yaml:"max_age"`            // 最大存活时间（默认30分钟）
	MaxSize          int           `yaml:"max_size"`           // 最大容量（默认10000）
	CleanupInterval  time.Duration `yaml:"cleanup_interval"`   // 清理间隔（默认1分钟）
	ArchiveOnCleanup bool          `yaml:"archive_on_cleanup"` // 清理时是否归档（默认true）
}

// WriteRequest 写操作请求
type WriteRequest struct {
	Query     string
	Args      []interface{}
	Response  chan error
	Context   context.Context
	EventType string // 用于调试和监控
}

// UpdateOptions 统一的请求更新选项
// 支持可选字段更新，只更新非nil的字段
type UpdateOptions struct {
	EndpointName  *string        // 端点名称
	Channel       *string        // 渠道标签（v5.0）
	GroupName     *string        // 组名称
	AuthType      *string        // 认证类型：token/api_key
	AuthKey       *string        // 认证标识（脱敏/指纹），不含明文
	Status        *string        // 状态
	RetryCount    *int           // 重试次数
	HttpStatus    *int           // HTTP状态码
	ModelName     *string        // 模型名称
	EndTime       *time.Time     // 结束时间
	Duration      *time.Duration // 持续时间
	FailureReason *string        // 失败原因（用于中间过程记录）
}

// UsageTracker 使用跟踪器
type UsageTracker struct {
	// 原有字段（兼容性）
	db           *sql.DB
	eventChan    chan RequestEvent
	config       *Config
	pricing      map[string]ModelPricing       // 模型定价缓存
	endpointMu   map[string]EndpointMultiplier // 端点倍率缓存
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.RWMutex
	errorHandler *ErrorHandler

	// 时区支持
	location *time.Location // 配置的时区

	// 新增：数据库适配器
	adapter DatabaseAdapter // 数据库适配器接口

	// 新增：读写分离组件（从适配器获取）
	readDB     *sql.DB           // 读连接池 (多连接)
	writeDB    *sql.DB           // 写连接 (单连接)
	writeQueue chan WriteRequest // 写操作队列
	writeMu    sync.Mutex        // 写操作保护锁
	writeWg    sync.WaitGroup    // 写处理器等待组

	// 🔥 v4.1 新增：内存热池 + 归档架构
	hotPool        *HotPool        // 内存热池（活跃请求）
	archiveManager *ArchiveManager // 归档管理器（批量写入）
	hotPoolEnabled bool            // 是否启用热池模式
}

// NewUsageTracker 创建新的使用跟踪器
func NewUsageTracker(config *Config, globalTimezone ...string) (*UsageTracker, error) {
	if config == nil || !config.Enabled {
		return &UsageTracker{config: config}, nil
	}

	// 设置默认值
	if config.BufferSize <= 0 {
		config.BufferSize = 1000
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = 30 * time.Second
	}
	if config.MaxRetry <= 0 {
		config.MaxRetry = 3
	}
	if config.CleanupInterval <= 0 {
		config.CleanupInterval = 24 * time.Hour // 默认24小时清理一次
	}

	// 构建数据库配置
	tz := ""
	if len(globalTimezone) > 0 {
		tz = globalTimezone[0]
	}
	dbConfig, err := buildDatabaseConfig(config, tz)
	if err != nil {
		return nil, fmt.Errorf("failed to build database config: %w", err)
	}

	// 创建数据库适配器
	adapter, err := NewDatabaseAdapter(dbConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create database adapter: %w", err)
	}

	// 打开数据库连接
	if err := adapter.Open(); err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// 获取读写连接（从适配器）
	readDB := adapter.GetReadDB()
	writeDB := adapter.GetWriteDB()

	// 保持原有db字段兼容性（指向readDB）
	db := readDB

	ctx, cancel := context.WithCancel(context.Background())

	// 初始化时区
	timezone := dbConfig.Timezone
	if timezone == "" {
		timezone = "Asia/Shanghai" // 默认时区
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		slog.Warn("加载时区失败，使用Asia/Shanghai", "timezone", timezone, "error", err)
		location, _ = time.LoadLocation("Asia/Shanghai")
		if location == nil {
			location = time.FixedZone("CST", 8*3600) // 后备方案：固定+8时区
		}
	}

	ut := &UsageTracker{
		// 原有字段（兼容性）
		db:        db, // 兼容性：指向readDB
		eventChan: make(chan RequestEvent, config.BufferSize),
		config:    config,
		pricing:   config.ModelPricing,
		ctx:       ctx,
		cancel:    cancel,

		// 时区支持
		location: location,

		// 新增：数据库适配器
		adapter: adapter,

		// 读写分离组件（从适配器获取）
		readDB:     readDB,
		writeDB:    writeDB,
		writeQueue: make(chan WriteRequest, config.BufferSize), // 与事件队列容量一致
	}

	// 初始化错误处理器
	ut.errorHandler = NewErrorHandler(ut, slog.Default())

	// 初始化数据库Schema（使用适配器）
	if err := ut.initDatabaseWithAdapter(); err != nil {
		cancel()
		adapter.Close()
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	// 启动写操作处理器
	go ut.processWriteQueue()

	// 启动异步事件处理器
	ut.wg.Add(1)
	go ut.processEvents()

	// 启动定期清理任务
	ut.wg.Add(1)
	go ut.periodicCleanup()

	// 启动定期备份任务
	ut.wg.Add(1)
	go ut.periodicBackup()

	// 🔥 v4.1 初始化热池架构
	ut.initHotPool()

	slog.Info("✅ 使用跟踪器初始化完成",
		"database_type", adapter.GetDatabaseType(),
		"buffer_size", config.BufferSize,
		"batch_size", config.BatchSize,
		"hot_pool_enabled", ut.hotPoolEnabled)

	return ut, nil
}

// initHotPool 初始化热池和归档管理器
func (ut *UsageTracker) initHotPool() {
	// 检查是否启用热池模式（默认启用）
	hotPoolEnabled := true
	var hotPoolConfig HotPoolConfig

	if ut.config.HotPool != nil {
		// 使用配置文件中的设置
		if !ut.config.HotPool.Enabled {
			hotPoolEnabled = false
		}
		hotPoolConfig = HotPoolConfig{
			MaxAge:           ut.config.HotPool.MaxAge,
			MaxSize:          ut.config.HotPool.MaxSize,
			CleanupInterval:  ut.config.HotPool.CleanupInterval,
			ArchiveOnCleanup: ut.config.HotPool.ArchiveOnCleanup,
		}
	} else {
		// 使用默认配置
		hotPoolConfig = DefaultHotPoolConfig()
	}

	if !hotPoolEnabled {
		slog.Info("🔥 热池模式已禁用，使用传统事件队列模式")
		ut.hotPoolEnabled = false
		return
	}

	// 创建热池
	ut.hotPool = NewHotPool(hotPoolConfig)

	// 创建归档管理器
	archiveConfig := ArchiveManagerConfig{
		ChannelSize:   ut.config.BufferSize,
		BatchSize:     ut.config.BatchSize,
		FlushInterval: ut.config.FlushInterval,
		MaxRetry:      ut.config.MaxRetry,
	}
	ut.archiveManager = NewArchiveManager(ut.adapter, archiveConfig, ut.pricing, ut.location)

	// 设置双向引用：ArchiveManager 需要访问 HotPool 来清理归档缓存
	ut.archiveManager.SetHotPool(ut.hotPool)

	// 设置热池归档回调
	ut.hotPool.SetArchiveCallback(func(req *ActiveRequest) {
		if ut.archiveManager != nil {
			ut.archiveManager.Archive(req)
		}
	})

	ut.hotPoolEnabled = true
	slog.Info("🔥 热池模式已启用",
		"max_age", hotPoolConfig.MaxAge,
		"max_size", hotPoolConfig.MaxSize)
}

// now 返回当前配置时区的时间
func (ut *UsageTracker) now() time.Time {
	if ut.location == nil {
		return time.Now() // 后备方案
	}
	return time.Now().In(ut.location)
}

// buildDatabaseConfig 从Config构建DatabaseConfig
// v4.1.0: 简化为仅支持 SQLite
func buildDatabaseConfig(config *Config, globalTimezone string) (DatabaseConfig, error) {
	var dbConfig DatabaseConfig

	// 设置数据库类型（v4.1+ 仅支持 SQLite）
	dbConfig.Type = "sqlite"

	// 优先使用新的Database配置
	if config.Database != nil {
		// 检查是否配置了 MySQL（已废弃）
		if config.Database.Type == "mysql" {
			slog.Warn("⚠️  MySQL 配置已废弃，将使用 SQLite",
				"reason", "v4.1.0 移除了 MySQL 支持",
				"suggestion", "请修改配置 type: sqlite 或删除 database.type 配置")
		}
		// 兼容：配置里写了 database 但没写 path 时，回退到 DatabasePath（由上层默认填充为用户目录路径）。
		if config.Database.Path != "" {
			dbConfig.DatabasePath = config.Database.Path
		} else {
			dbConfig.DatabasePath = config.DatabasePath
		}
		if config.Database.Timezone != "" {
			dbConfig.Timezone = config.Database.Timezone
		} else {
			dbConfig.Timezone = globalTimezone
		}
	} else {
		// 向后兼容：使用原有的DatabasePath配置
		dbConfig.DatabasePath = config.DatabasePath
	}

	// 设置默认数据库路径 - 使用跨平台用户目录
	if dbConfig.DatabasePath == "" {
		// 使用 internal/utils 提供的跨平台目录
		// Windows: %APPDATA%\CC-Forwarder\data\cc-forwarder.db
		// macOS: ~/Library/Application Support/CC-Forwarder/data/cc-forwarder.db
		// Linux: ~/.local/share/cc-forwarder/data/cc-forwarder.db
		dbConfig.DatabasePath = filepath.Join(getAppDataDir(), "data", "cc-forwarder.db")
	}

	// 时区级联逻辑：优先级 database.timezone > global.timezone > 默认值
	if dbConfig.Timezone == "" {
		if globalTimezone != "" {
			dbConfig.Timezone = globalTimezone
		}
		// setDefaultConfig 会设置默认值 Asia/Shanghai
	}

	return dbConfig, nil
}

// getAppDataDir 获取应用数据目录（跨平台）
// 复制自 internal/utils/appdir.go，避免循环依赖
// Windows: %APPDATA%\CC-Forwarder
// macOS: ~/Library/Application Support/CC-Forwarder
// Linux: ~/.local/share/cc-forwarder
func getAppDataDir() string {
	var baseDir string

	switch runtime.GOOS {
	case "windows":
		baseDir = os.Getenv("APPDATA")
		if baseDir == "" {
			baseDir = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
		}
		return filepath.Join(baseDir, "CC-Forwarder")

	case "darwin":
		homeDir, _ := os.UserHomeDir()
		return filepath.Join(homeDir, "Library", "Application Support", "CC-Forwarder")

	case "linux":
		homeDir, _ := os.UserHomeDir()
		xdgDataHome := os.Getenv("XDG_DATA_HOME")
		if xdgDataHome != "" {
			return filepath.Join(xdgDataHome, "cc-forwarder")
		}
		return filepath.Join(homeDir, ".local", "share", "cc-forwarder")

	default:
		homeDir, _ := os.UserHomeDir()
		return filepath.Join(homeDir, ".cc-forwarder")
	}
}

// initDatabaseWithAdapter 使用适配器初始化数据库
func (ut *UsageTracker) initDatabaseWithAdapter() error {
	if ut.adapter == nil {
		return fmt.Errorf("database adapter not initialized")
	}

	// 使用适配器初始化Schema
	if err := ut.adapter.InitSchema(); err != nil {
		return fmt.Errorf("failed to initialize database schema: %w", err)
	}

	slog.Info("数据库Schema初始化完成",
		"database_type", ut.adapter.GetDatabaseType())

	return nil
}

// Close 关闭使用跟踪器
func (ut *UsageTracker) Close() error {
	if ut.config == nil || !ut.config.Enabled {
		return nil
	}

	// 先检查是否已经关闭
	ut.mu.RLock()
	if ut.cancel == nil {
		ut.mu.RUnlock()
		return nil // 已经关闭过
	}
	ut.mu.RUnlock()

	slog.Info("Shutting down usage tracker...")

	// 取消上下文（不需要持有锁）
	ut.cancel()

	// 等待所有协程完成（包括写处理器）
	ut.wg.Wait()
	ut.writeWg.Wait() // 等待写处理器完成

	// 现在可以安全地持有写锁进行清理
	ut.mu.Lock()
	defer ut.mu.Unlock()

	ut.cancel = nil // 标记为已关闭

	// 关闭事件通道
	if ut.eventChan != nil {
		close(ut.eventChan)
		ut.eventChan = nil
	}

	// 关闭写操作队列
	if ut.writeQueue != nil {
		close(ut.writeQueue)
		ut.writeQueue = nil
	}

	// 🔥 v4.1 关闭热池和归档管理器
	if ut.hotPool != nil {
		if err := ut.hotPool.Close(); err != nil {
			slog.Error("Failed to close hot pool", "error", err)
		}
		ut.hotPool = nil
	}
	if ut.archiveManager != nil {
		if err := ut.archiveManager.Close(); err != nil {
			slog.Error("Failed to close archive manager", "error", err)
		}
		ut.archiveManager = nil
	}

	// 关闭数据库适配器（会自动处理所有连接）
	if ut.adapter != nil {
		if err := ut.adapter.Close(); err != nil {
			slog.Error("Failed to close database adapter", "error", err)
			return fmt.Errorf("failed to close database adapter: %w", err)
		}
		ut.adapter = nil
	}

	// 清理连接引用（这些现在由adapter管理）
	ut.readDB = nil
	ut.writeDB = nil
	ut.db = nil

	slog.Info("✅ 使用跟踪器关闭完成")
	return nil
}

// RecordRequestStart 记录请求开始
func (ut *UsageTracker) RecordRequestStart(requestID, clientIP, userAgent, method, path string, isStreaming bool) {
	if ut.config == nil || !ut.config.Enabled {
		return
	}

	// 🔥 v4.1 热池模式：直接添加到内存热池
	if ut.hotPoolEnabled && ut.hotPool != nil {
		req := NewActiveRequest(requestID, clientIP, userAgent, method, path, isStreaming)
		if err := ut.hotPool.Add(req); err != nil {
			slog.Warn("🔥 热池添加请求失败，降级到事件队列模式",
				"request_id", requestID,
				"error", err)
			// 降级到传统模式
			ut.recordRequestStartLegacy(requestID, clientIP, userAgent, method, path, isStreaming)
		}
		return
	}

	// 传统模式：发送事件到队列
	ut.recordRequestStartLegacy(requestID, clientIP, userAgent, method, path, isStreaming)
}

// recordRequestStartLegacy 传统模式记录请求开始
func (ut *UsageTracker) recordRequestStartLegacy(requestID, clientIP, userAgent, method, path string, isStreaming bool) {
	event := RequestEvent{
		Type:      "start",
		RequestID: requestID,
		Timestamp: ut.now(),
		Data: RequestStartData{
			ClientIP:    clientIP,
			UserAgent:   userAgent,
			Method:      method,
			Path:        path,
			IsStreaming: isStreaming,
		},
	}

	select {
	case ut.eventChan <- event:
		// 成功发送事件
	default:
		// 缓冲区满时的处理策略
		slog.Warn("Usage tracking event buffer full, dropping start event",
			"request_id", requestID)
	}
}

// RecordRequestUpdate 统一的请求更新方法
// 支持可选字段更新，只更新非nil的字段，适用于所有中间过程状态变更
func (ut *UsageTracker) RecordRequestUpdate(requestID string, opts UpdateOptions) {
	if ut.config == nil || !ut.config.Enabled {
		return
	}

	// 🔥 v4.1 热池模式：直接更新内存（核心优化点：无数据库写入）
	if ut.hotPoolEnabled && ut.hotPool != nil {
		err := ut.hotPool.Update(requestID, func(req *ActiveRequest) {
			// 只更新非nil的字段
			if opts.EndpointName != nil {
				req.EndpointName = *opts.EndpointName
			}
			if opts.Channel != nil {
				req.Channel = *opts.Channel
			}
			if opts.GroupName != nil {
				req.GroupName = *opts.GroupName
			}
			if opts.AuthType != nil {
				req.AuthType = *opts.AuthType
			}
			if opts.AuthKey != nil {
				req.AuthKey = *opts.AuthKey
			}
			if opts.Status != nil {
				req.Status = *opts.Status
			}
			if opts.RetryCount != nil {
				req.RetryCount = *opts.RetryCount
			}
			if opts.HttpStatus != nil {
				req.HTTPStatus = *opts.HttpStatus
			}
			if opts.ModelName != nil {
				req.ModelName = *opts.ModelName
			}
			if opts.FailureReason != nil {
				req.FailureReason = *opts.FailureReason
			}
		})
		if err != nil {
			// 请求可能不在热池中（已归档或从未记录），降级到传统模式
			slog.Debug("🔥 热池更新请求失败，降级到事件队列模式",
				"request_id", requestID,
				"error", err)
			ut.recordRequestUpdateLegacy(requestID, opts)
		}
		return
	}

	// 传统模式：发送事件到队列
	ut.recordRequestUpdateLegacy(requestID, opts)
}

// recordRequestUpdateLegacy 传统模式更新请求
func (ut *UsageTracker) recordRequestUpdateLegacy(requestID string, opts UpdateOptions) {
	event := RequestEvent{
		Type:      "flexible_update",
		RequestID: requestID,
		Timestamp: ut.now(),
		Data:      opts,
	}

	select {
	case ut.eventChan <- event:
		// 成功发送事件
	default:
		slog.Warn("Usage tracking event buffer full, dropping flexible_update event",
			"request_id", requestID)
	}
}

// RecordRequestSuccess 记录请求成功完成
// 一次性更新所有成功相关字段：status='completed', end_time, duration_ms, Token和成本信息
func (ut *UsageTracker) RecordRequestSuccess(requestID, modelName string, tokens *TokenUsage, duration time.Duration) {
	ut.RecordRequestSuccessWithQuality(requestID, modelName, tokens, duration, "")
}

// RecordRequestSuccessWithQuality 记录请求成功完成（支持数据质量标记）
// 🔧 [方案A实现] 2025-12-20: 原子操作，在 CompleteAndArchive 中一次性设置所有字段包括 failureReason
// 一次性更新所有成功相关字段：status='completed', end_time, duration_ms, Token、成本信息和可选的 failure_reason
func (ut *UsageTracker) RecordRequestSuccessWithQuality(requestID, modelName string, tokens *TokenUsage, duration time.Duration, failureReason string) {
	if ut.config == nil || !ut.config.Enabled {
		return
	}

	// 🚀 [架构修复] 支持 nil tokens，确保耗时信息总是被记录
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64
	var cacheCreation5mTokens, cacheCreation1hTokens int64 // v5.0.1+
	if tokens != nil {
		inputTokens = tokens.InputTokens
		outputTokens = tokens.OutputTokens
		cacheCreationTokens = tokens.CacheCreationTokens
		cacheCreation5mTokens = tokens.CacheCreation5mTokens
		cacheCreation1hTokens = tokens.CacheCreation1hTokens
		cacheReadTokens = tokens.CacheReadTokens

		// 🔧 [向后兼容修复] v5.0.1+: 如果 5m/1h 都是 0 但总数不为 0，
		// 将总数分配给 5m 字段，与成本计算逻辑保持一致（tracker.go:185-193）
		if cacheCreation5mTokens == 0 && cacheCreation1hTokens == 0 && cacheCreationTokens > 0 {
			cacheCreation5mTokens = cacheCreationTokens // 默认按 5m 缓存处理
			slog.Debug("🔄 [缓存Token分配] 向后兼容模式：将总缓存Token分配到5m字段",
				"request_id", requestID,
				"cache_creation_tokens", cacheCreationTokens)
		}
	}
	// 如果 tokens 为 nil，所有 token 字段都是 0，但 duration 仍然会被记录

	// 🔥 v4.1 热池模式：完成请求并归档（核心：单次数据库写入）
	if ut.hotPoolEnabled && ut.hotPool != nil {
		now := ut.now()
		err := ut.hotPool.CompleteAndArchive(requestID, func(req *ActiveRequest) {
			req.Status = "completed"
			req.ModelName = modelName
			req.InputTokens = inputTokens
			req.OutputTokens = outputTokens
			req.CacheCreationTokens = cacheCreationTokens
			req.CacheCreation5mTokens = cacheCreation5mTokens // v5.0.1+
			req.CacheCreation1hTokens = cacheCreation1hTokens // v5.0.1+
			req.CacheReadTokens = cacheReadTokens
			req.EndTime = &now
			req.DurationMs = duration.Milliseconds()
			// 🔧 [方案A实现] 2025-12-20: 显式覆盖 failureReason（无论是否为空）
			// 避免之前中途错误设置的旧值残留，导致"成功但带失败原因"的误标
			req.FailureReason = failureReason
			// 成本在归档时计算
		})
		if err != nil {
			// 请求可能不在热池中，降级到传统模式
			slog.Debug("🔥 热池完成请求失败，降级到事件队列模式",
				"request_id", requestID,
				"error", err)
			ut.recordRequestSuccessLegacy(requestID, modelName, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, duration, failureReason)
		}
		return
	}

	// 传统模式：发送事件到队列
	ut.recordRequestSuccessLegacy(requestID, modelName, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, duration, failureReason)
}

// recordRequestSuccessLegacy 传统模式记录请求成功
// 🔧 [方案A实现] 2025-12-20: 增加 failureReason 参数支持
func (ut *UsageTracker) recordRequestSuccessLegacy(requestID, modelName string, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64, duration time.Duration, failureReason string) {
	event := RequestEvent{
		Type:      "success",
		RequestID: requestID,
		Timestamp: ut.now(),
		Data: RequestCompleteData{
			ModelName:           modelName,
			InputTokens:         inputTokens,
			OutputTokens:        outputTokens,
			CacheCreationTokens: cacheCreationTokens,
			CacheReadTokens:     cacheReadTokens,
			Duration:            duration,
			FailureReason:       failureReason,
		},
	}

	select {
	case ut.eventChan <- event:
		// 成功发送事件
	default:
		slog.Warn("Usage tracking event buffer full, dropping success event",
			"request_id", requestID)
	}
}

// RecordRequestFinalFailure 记录请求最终失败或取消
// 一次性更新所有失败/取消相关字段：status, end_time, duration_ms, failure_reason/cancel_reason, http_status_code, 可选Token
// 🔧 [修复] 2025-12-11: 添加 modelName 参数，确保取消/失败请求能正确计算成本
func (ut *UsageTracker) RecordRequestFinalFailure(requestID, modelName, status, reason, errorDetail string, duration time.Duration, httpStatus int, tokens *TokenUsage) {
	if ut.config == nil || !ut.config.Enabled {
		return
	}

	// 处理Token信息（失败/取消时可能有也可能没有）
	var inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64
	var cacheCreation5mTokens, cacheCreation1hTokens int64 // 🔧 [修复] 2025-12-11: 添加 5m/1h 缓存字段
	if tokens != nil {
		inputTokens = tokens.InputTokens
		outputTokens = tokens.OutputTokens
		cacheCreationTokens = tokens.CacheCreationTokens
		cacheCreation5mTokens = tokens.CacheCreation5mTokens
		cacheCreation1hTokens = tokens.CacheCreation1hTokens
		cacheReadTokens = tokens.CacheReadTokens

		// 🔧 [向后兼容修复] v5.0.1+: 如果 5m/1h 都是 0 但总数不为 0，
		// 将总数分配给 5m 字段，与成本计算逻辑保持一致
		if cacheCreation5mTokens == 0 && cacheCreation1hTokens == 0 && cacheCreationTokens > 0 {
			cacheCreation5mTokens = cacheCreationTokens
		}
	}

	// 🔥 v4.1 热池模式：完成失败请求并归档
	if ut.hotPoolEnabled && ut.hotPool != nil {
		now := ut.now()
		err := ut.hotPool.CompleteAndArchive(requestID, func(req *ActiveRequest) {
			req.Status = status
			req.HTTPStatus = httpStatus
			// 🔧 [修复] 2025-12-11: 设置模型名，否则成本计算会失败
			if modelName != "" && modelName != "unknown" {
				req.ModelName = modelName
			}
			// 🔧 [修复] 2025-12-15: 只有当 tokens 参数不为 nil 时才更新 token 字段
			// 否则保留热池中已有的值（可能由 RecordFailedRequestTokens 设置）
			if tokens != nil {
				req.InputTokens = inputTokens
				req.OutputTokens = outputTokens
				req.CacheCreationTokens = cacheCreationTokens
				req.CacheCreation5mTokens = cacheCreation5mTokens
				req.CacheCreation1hTokens = cacheCreation1hTokens
				req.CacheReadTokens = cacheReadTokens
			}
			req.EndTime = &now
			req.DurationMs = duration.Milliseconds()
			// 根据状态设置原因字段
			if status == "cancelled" {
				req.CancelReason = reason
			} else {
				req.FailureReason = reason
				if errorDetail != "" {
					req.FailureReason = reason + ": " + errorDetail
				}
			}
		})
		if err != nil {
			// 请求可能不在热池中，降级到传统模式
			slog.Debug("🔥 热池完成失败请求失败，降级到事件队列模式",
				"request_id", requestID,
				"error", err)
			ut.recordRequestFinalFailureLegacy(requestID, modelName, status, reason, errorDetail, duration, httpStatus, inputTokens, outputTokens, cacheCreationTokens, cacheCreation5mTokens, cacheCreation1hTokens, cacheReadTokens)
		}
		return
	}

	// 传统模式：发送事件到队列
	ut.recordRequestFinalFailureLegacy(requestID, modelName, status, reason, errorDetail, duration, httpStatus, inputTokens, outputTokens, cacheCreationTokens, cacheCreation5mTokens, cacheCreation1hTokens, cacheReadTokens)
}

// recordRequestFinalFailureLegacy 传统模式记录请求失败
// 🔧 [修复] 2025-12-11: 添加 modelName 和 5m/1h 缓存字段参数
func (ut *UsageTracker) recordRequestFinalFailureLegacy(requestID, modelName, status, reason, errorDetail string, duration time.Duration, httpStatus int, inputTokens, outputTokens, cacheCreationTokens, cacheCreation5mTokens, cacheCreation1hTokens, cacheReadTokens int64) {
	event := RequestEvent{
		Type:      "final_failure",
		RequestID: requestID,
		Timestamp: ut.now(),
		Data: map[string]interface{}{
			"status":                   status, // "failed" or "cancelled"
			"reason":                   reason, // failure_reason or cancel_reason
			"error_detail":             errorDetail,
			"duration":                 duration,
			"http_status":              httpStatus, // HTTP状态码
			"model_name":               modelName,  // 🔧 [修复] 2025-12-11: 添加模型名
			"input_tokens":             inputTokens,
			"output_tokens":            outputTokens,
			"cache_creation_tokens":    cacheCreationTokens,
			"cache_creation_5m_tokens": cacheCreation5mTokens, // 🔧 [修复] 2025-12-11
			"cache_creation_1h_tokens": cacheCreation1hTokens, // 🔧 [修复] 2025-12-11
			"cache_read_tokens":        cacheReadTokens,
		},
	}

	select {
	case ut.eventChan <- event:
		// 成功发送事件
	default:
		slog.Warn("Usage tracking event buffer full, dropping final_failure event",
			"request_id", requestID)
	}
}

// RecordFailedRequestTokens 记录失败请求的Token使用
// 只记录Token统计，不影响请求状态
func (ut *UsageTracker) RecordFailedRequestTokens(requestID, modelName string, tokens *TokenUsage, duration time.Duration, failureReason string) {
	if ut.config == nil || !ut.config.Enabled || tokens == nil {
		return
	}

	// 🔥 v4.1 热池模式：直接更新内存中的请求数据
	// 这样后续的 CompleteAndArchive 会将正确的 token 信息写入数据库
	if ut.hotPoolEnabled && ut.hotPool != nil {
		err := ut.hotPool.Update(requestID, func(req *ActiveRequest) {
			// 更新 Token 信息
			req.InputTokens = tokens.InputTokens
			req.OutputTokens = tokens.OutputTokens
			req.CacheCreationTokens = tokens.CacheCreationTokens
			req.CacheCreation5mTokens = tokens.CacheCreation5mTokens
			req.CacheCreation1hTokens = tokens.CacheCreation1hTokens
			req.CacheReadTokens = tokens.CacheReadTokens
			// 更新模型名（如果有）
			if modelName != "" && modelName != "unknown" {
				req.ModelName = modelName
			}
			// 更新持续时间
			if duration > 0 {
				req.DurationMs = duration.Milliseconds()
			}
			// 记录失败原因（不改变状态）
			if failureReason != "" && req.FailureReason == "" {
				req.FailureReason = failureReason
			}
		})
		if err == nil {
			slog.Debug(fmt.Sprintf("🔥 [热池Token更新] [%s] 原因: %s, 模型: %s, 输入: %d, 输出: %d",
				requestID, failureReason, modelName, tokens.InputTokens, tokens.OutputTokens))
			return
		}
		// 请求不在热池中（可能已归档），降级到传统模式
		slog.Debug(fmt.Sprintf("🔥 [热池Token更新失败] [%s] 降级到事件队列模式, 错误: %v",
			requestID, err))
	}

	// 传统模式：发送事件到队列（UPDATE 已存在的数据库记录）
	event := RequestEvent{
		Type:      "failed_request_tokens",
		RequestID: requestID,
		Timestamp: ut.now(),
		Data: RequestCompleteData{
			ModelName:             modelName,
			InputTokens:           tokens.InputTokens,
			OutputTokens:          tokens.OutputTokens,
			CacheCreationTokens:   tokens.CacheCreationTokens,
			CacheCreation5mTokens: tokens.CacheCreation5mTokens,
			CacheCreation1hTokens: tokens.CacheCreation1hTokens,
			CacheReadTokens:       tokens.CacheReadTokens,
			Duration:              duration,
			FailureReason:         failureReason,
		},
	}

	select {
	case ut.eventChan <- event:
		slog.Debug(fmt.Sprintf("💾 [失败Token事件] [%s] 原因: %s, 模型: %s", requestID, failureReason, modelName))
	default:
		slog.Warn("Usage tracking event buffer full, dropping failed request tokens event",
			"request_id", requestID)
	}
}

// RecoverRequestTokens 恢复请求的Token使用统计
// 🔧 [Fallback修复] 专用于debug文件恢复场景，仅更新Token字段，不触发状态变更
func (ut *UsageTracker) RecoverRequestTokens(requestID, modelName string, tokens *TokenUsage) {
	if ut.config == nil || !ut.config.Enabled || tokens == nil {
		return
	}

	// 🔥 v4.1 热池模式：如果请求还在热池中，先更新热池
	if ut.hotPoolEnabled && ut.hotPool != nil {
		err := ut.hotPool.Update(requestID, func(req *ActiveRequest) {
			// 更新 Token 信息
			req.InputTokens = tokens.InputTokens
			req.OutputTokens = tokens.OutputTokens
			req.CacheCreationTokens = tokens.CacheCreationTokens
			req.CacheCreation5mTokens = tokens.CacheCreation5mTokens
			req.CacheCreation1hTokens = tokens.CacheCreation1hTokens
			req.CacheReadTokens = tokens.CacheReadTokens
			// 更新模型名（如果有）
			if modelName != "" && modelName != "unknown" {
				req.ModelName = modelName
			}
		})
		if err == nil {
			slog.Info(fmt.Sprintf("🔥 [热池Token恢复] [%s] 模型: %s, 输入: %d, 输出: %d",
				requestID, modelName, tokens.InputTokens, tokens.OutputTokens))
			return
		}
		// 请求不在热池中（已归档），继续使用传统模式更新数据库
		slog.Debug(fmt.Sprintf("🔥 [热池Token恢复失败] [%s] 请求已归档，使用UPDATE更新数据库",
			requestID))
	}

	// 传统模式：发送事件到队列（UPDATE 已存在的数据库记录）
	event := RequestEvent{
		Type:      "token_recovery",
		RequestID: requestID,
		Timestamp: ut.now(),
		Data: RequestCompleteData{
			ModelName:             modelName,
			InputTokens:           tokens.InputTokens,
			OutputTokens:          tokens.OutputTokens,
			CacheCreationTokens:   tokens.CacheCreationTokens,
			CacheCreation5mTokens: tokens.CacheCreation5mTokens,
			CacheCreation1hTokens: tokens.CacheCreation1hTokens,
			CacheReadTokens:       tokens.CacheReadTokens,
			Duration:              0, // 不更新时间相关字段
		},
	}

	select {
	case ut.eventChan <- event:
		slog.Info(fmt.Sprintf("🔧 [Token恢复事件] [%s] 模型: %s, 输入: %d, 输出: %d",
			requestID, modelName, tokens.InputTokens, tokens.OutputTokens))
	default:
		slog.Warn("Usage tracking event buffer full, dropping token recovery event",
			"request_id", requestID)
	}
}

// UpdatePricing 更新模型定价（运行时动态更新）
func (ut *UsageTracker) UpdatePricing(pricing map[string]ModelPricing) {
	ut.mu.Lock()
	defer ut.mu.Unlock()

	ut.pricing = pricing

	// 同步到 ArchiveManager
	if ut.archiveManager != nil {
		ut.archiveManager.UpdatePricing(pricing)
	}

	slog.Info("Model pricing updated", "model_count", len(pricing))
}

// UpdateEndpointMultipliers 更新端点成本倍率
// 成本计算公式：模型基础定价 * 端点倍率
func (ut *UsageTracker) UpdateEndpointMultipliers(multipliers map[string]EndpointMultiplier) {
	ut.mu.Lock()
	defer ut.mu.Unlock()

	ut.endpointMu = multipliers

	// 同步到 ArchiveManager
	if ut.archiveManager != nil {
		ut.archiveManager.UpdateEndpointMultipliers(multipliers)
	}

	slog.Info("Endpoint multipliers updated", "endpoint_count", len(multipliers))
}

// GetEndpointMultiplier 获取端点倍率（用于成本计算）
func (ut *UsageTracker) GetEndpointMultiplier(endpointName string) EndpointMultiplier {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	if ut.endpointMu == nil {
		return EndpointMultiplier{
			CostMultiplier:                1.0,
			InputCostMultiplier:           1.0,
			OutputCostMultiplier:          1.0,
			CacheCreationCostMultiplier:   1.0,
			CacheCreationCostMultiplier1h: 1.0,
			CacheReadCostMultiplier:       1.0,
		}
	}

	if m, exists := ut.endpointMu[endpointName]; exists {
		return m
	}

	// 返回默认倍率 1.0
	return EndpointMultiplier{
		CostMultiplier:                1.0,
		InputCostMultiplier:           1.0,
		OutputCostMultiplier:          1.0,
		CacheCreationCostMultiplier:   1.0,
		CacheCreationCostMultiplier1h: 1.0,
		CacheReadCostMultiplier:       1.0,
	}
}

// GetDatabaseStats 获取数据库统计信息（包装方法）
func (ut *UsageTracker) GetDatabaseStats(ctx context.Context) (*DatabaseStats, error) {
	if ut.config == nil || !ut.config.Enabled {
		return nil, fmt.Errorf("usage tracking not enabled")
	}
	if ut.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}
	return ut.getDatabaseStatsInternal(ctx)
}

// HealthCheck 检查数据库连接状态和基本功能（使用读连接）
func (ut *UsageTracker) HealthCheck(ctx context.Context) error {
	if ut.config == nil || !ut.config.Enabled {
		return nil // 如果未启用，认为是健康的
	}

	if ut.readDB == nil {
		return fmt.Errorf("read database not initialized")
	}

	// 测试读数据库连接
	if err := ut.readDB.PingContext(ctx); err != nil {
		return fmt.Errorf("read database ping failed: %w", err)
	}

	// 测试写数据库连接
	if ut.writeDB != nil {
		if err := ut.writeDB.PingContext(ctx); err != nil {
			return fmt.Errorf("write database ping failed: %w", err)
		}
	}

	// 测试基本查询（使用读连接）
	var count int
	err := ut.readDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&count)
	if err != nil {
		return fmt.Errorf("database query test failed: %w", err)
	}

	// 检查表是否存在
	if count < 2 { // 至少应该有 request_logs 和 usage_summary 两个表
		return fmt.Errorf("database schema incomplete: expected at least 2 tables, found %d", count)
	}

	// 检查事件处理通道是否正常
	select {
	case <-ut.ctx.Done():
		return fmt.Errorf("usage tracker context cancelled")
	default:
		// 上下文正常
	}

	// 检查事件通道容量
	if ut.eventChan != nil {
		channelLoad := float64(len(ut.eventChan)) / float64(cap(ut.eventChan)) * 100
		if channelLoad > 90 {
			return fmt.Errorf("event channel overloaded: %.1f%% capacity used", channelLoad)
		}
	}

	// 检查写队列容量
	if ut.writeQueue != nil {
		writeQueueLoad := float64(len(ut.writeQueue)) / float64(cap(ut.writeQueue)) * 100
		if writeQueueLoad > 90 {
			return fmt.Errorf("write queue overloaded: %.1f%% capacity used", writeQueueLoad)
		}
	}

	return nil
}

// ForceFlush 强制刷新所有待处理事件
func (ut *UsageTracker) ForceFlush() error {
	if ut.config == nil || !ut.config.Enabled {
		return nil
	}

	// 尝试发送一个特殊事件来触发批处理
	flushEvent := RequestEvent{
		Type:      "flush",
		RequestID: "force-flush-" + ut.now().Format("20060102150405"),
		Timestamp: ut.now(),
		Data:      nil,
	}

	select {
	case ut.eventChan <- flushEvent:
		slog.Info("Force flush event sent")
		return nil
	default:
		return fmt.Errorf("event channel full, cannot force flush")
	}
}

// GetPricing 获取模型定价
func (ut *UsageTracker) GetPricing(modelName string) ModelPricing {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	if pricing, exists := ut.pricing[modelName]; exists {
		return pricing
	}
	return ut.config.DefaultPricing
}

// GetConfiguredModels 获取配置中的所有模型列表
func (ut *UsageTracker) GetConfiguredModels() []string {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	models := make([]string, 0, len(ut.pricing))
	for modelName := range ut.pricing {
		models = append(models, modelName)
	}

	return models
}

// GetUsageSummary 获取使用摘要（便利方法）
func (ut *UsageTracker) GetUsageSummary(ctx context.Context, startTime, endTime time.Time) ([]UsageSummary, error) {
	opts := &QueryOptions{
		StartDate: &startTime,
		EndDate:   &endTime,
		Limit:     100,
	}
	return ut.QueryUsageSummary(ctx, opts)
}

// GetRequestLogs 获取请求日志（便利方法）
func (ut *UsageTracker) GetRequestLogs(ctx context.Context, startTime, endTime time.Time, modelName, endpointName, groupName string, limit, offset int) ([]RequestDetail, error) {
	opts := &QueryOptions{
		StartDate:    &startTime,
		EndDate:      &endTime,
		ModelName:    modelName,
		EndpointName: endpointName,
		GroupName:    groupName,
		Limit:        limit,
		Offset:       offset,
	}
	return ut.QueryRequestDetails(ctx, opts)
}

// GetUsageStats 获取使用统计（便利方法，使用读连接）
func (ut *UsageTracker) GetUsageStats(ctx context.Context, startTime, endTime time.Time) (*UsageStatsDetailed, error) {
	if ut.readDB == nil {
		return nil, fmt.Errorf("read database not initialized")
	}

	query := `SELECT 
		COUNT(*) as total_requests,
		SUM(CASE WHEN status IN ('completed', 'processing') THEN 1 ELSE 0 END) as success_requests,
		SUM(CASE WHEN status IN ('failed', 'error', 'auth_error', 'rate_limited', 'server_error', 'network_error', 'stream_error', 'timeout') THEN 1 ELSE 0 END) as error_requests,
		SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens) as total_tokens,
		SUM(total_cost_usd) as total_cost
		FROM request_logs 
		WHERE start_time >= ? AND start_time <= ?`

	var stats UsageStatsDetailed
	err := ut.readDB.QueryRowContext(ctx, query, startTime, endTime).Scan(
		&stats.TotalRequests,
		&stats.SuccessRequests,
		&stats.ErrorRequests,
		&stats.TotalTokens,
		&stats.TotalCost,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query detailed usage stats: %w", err)
	}

	// 获取模型统计（使用读连接）
	modelQuery := `SELECT model_name, COUNT(*), SUM(total_cost_usd)
		FROM request_logs 
		WHERE start_time >= ? AND start_time <= ? AND model_name IS NOT NULL AND model_name != ''
		GROUP BY model_name`

	rows, err := ut.readDB.QueryContext(ctx, modelQuery, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("failed to query model stats: %w", err)
	}
	defer rows.Close()

	stats.ModelStats = make(map[string]ModelStat)
	for rows.Next() {
		var modelName string
		var requests int64
		var cost float64
		if err := rows.Scan(&modelName, &requests, &cost); err != nil {
			continue
		}
		stats.ModelStats[modelName] = ModelStat{
			RequestCount: requests,
			TotalCost:    cost,
		}
	}

	// 获取端点统计（使用读连接）
	endpointQuery := `SELECT endpoint_name, COUNT(*), SUM(total_cost_usd)
		FROM request_logs 
		WHERE start_time >= ? AND start_time <= ? AND endpoint_name IS NOT NULL AND endpoint_name != ''
		GROUP BY endpoint_name`

	rows2, err := ut.readDB.QueryContext(ctx, endpointQuery, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("failed to query endpoint stats: %w", err)
	}
	defer rows2.Close()

	stats.EndpointStats = make(map[string]EndpointStat)
	for rows2.Next() {
		var endpointName string
		var requests int64
		var cost float64
		if err := rows2.Scan(&endpointName, &requests, &cost); err != nil {
			continue
		}
		stats.EndpointStats[endpointName] = EndpointStat{
			RequestCount: requests,
			TotalCost:    cost,
		}
	}

	// 获取组统计（使用读连接）
	groupQuery := `SELECT group_name, COUNT(*), SUM(total_cost_usd)
		FROM request_logs 
		WHERE start_time >= ? AND start_time <= ? AND group_name IS NOT NULL AND group_name != ''
		GROUP BY group_name`

	rows3, err := ut.readDB.QueryContext(ctx, groupQuery, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("failed to query group stats: %w", err)
	}
	defer rows3.Close()

	stats.GroupStats = make(map[string]GroupStat)
	for rows3.Next() {
		var groupName string
		var requests int64
		var cost float64
		if err := rows3.Scan(&groupName, &requests, &cost); err != nil {
			continue
		}
		stats.GroupStats[groupName] = GroupStat{
			RequestCount: requests,
			TotalCost:    cost,
		}
	}

	return &stats, nil
}

// ExportToCSV 导出为CSV格式
func (ut *UsageTracker) ExportToCSV(ctx context.Context, startTime, endTime time.Time, modelName, endpointName, groupName string) ([]byte, error) {
	logs, err := ut.GetRequestLogs(ctx, startTime, endTime, modelName, endpointName, groupName, 10000, 0) // Export up to 10k records
	if err != nil {
		return nil, fmt.Errorf("failed to get request logs for CSV export: %w", err)
	}

	// CSV header
	csv := "request_id,client_ip,user_agent,method,path,start_time,end_time,duration_ms,channel,endpoint_name,group_name,model_name,auth_type,auth_key,status,http_status_code,retry_count,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,input_cost_usd,output_cost_usd,cache_creation_cost_usd,cache_read_cost_usd,total_cost_usd,created_at,updated_at\n"

	// CSV rows
	for _, log := range logs {
		endTime := ""
		if log.EndTime != nil {
			endTime = log.EndTime.Format(time.RFC3339)
		}

		durationMs := ""
		if log.DurationMs != nil {
			durationMs = fmt.Sprintf("%d", *log.DurationMs)
		}

		httpStatus := ""
		if log.HTTPStatusCode != nil {
			httpStatus = fmt.Sprintf("%d", *log.HTTPStatusCode)
		}

		csv += fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%d,%d,%d,%d,%d,%.6f,%.6f,%.6f,%.6f,%.6f,%s,%s\n",
			log.RequestID, log.ClientIP, log.UserAgent, log.Method, log.Path,
			log.StartTime.Format(time.RFC3339), endTime, durationMs,
			log.Channel, log.EndpointName, log.GroupName, log.ModelName, log.AuthType, log.AuthKey, log.Status,
			httpStatus, log.RetryCount,
			log.InputTokens, log.OutputTokens, log.CacheCreationTokens, log.CacheReadTokens,
			log.InputCostUSD, log.OutputCostUSD, log.CacheCreationCostUSD, log.CacheReadCostUSD, log.TotalCostUSD,
			log.CreatedAt.Format(time.RFC3339), log.UpdatedAt.Format(time.RFC3339),
		)
	}

	return []byte(csv), nil
}

// ExportToJSON 导出为JSON格式
func (ut *UsageTracker) ExportToJSON(ctx context.Context, startTime, endTime time.Time, modelName, endpointName, groupName string) ([]byte, error) {
	logs, err := ut.GetRequestLogs(ctx, startTime, endTime, modelName, endpointName, groupName, 10000, 0) // Export up to 10k records
	if err != nil {
		return nil, fmt.Errorf("failed to get request logs for JSON export: %w", err)
	}

	// 使用标准库的json包序列化
	jsonBytes, err := json.Marshal(logs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal logs to JSON: %w", err)
	}

	return jsonBytes, nil
}

// processWriteQueue 启动写操作队列处理器（简化版，确保稳定性）
func (ut *UsageTracker) processWriteQueue() {
	ut.writeWg.Add(1)
	defer ut.writeWg.Done()
	slog.Debug("Write processor started")

	for {
		select {
		case writeReq := <-ut.writeQueue:
			err := ut.executeWriteSimple(writeReq)
			writeReq.Response <- err

		case <-ut.ctx.Done():
			// 退出前：尽量唤醒所有等待中的写请求，避免关闭时出现 goroutine 卡死
			drainErr := ut.ctx.Err()
			for {
				select {
				case writeReq := <-ut.writeQueue:
					writeReq.Response <- drainErr
				default:
					slog.Debug("Write processor stopped")
					return
				}
			}
		}
	}
}

// executeWriteSimple 执行简单写操作（避免复杂的批处理）
func (ut *UsageTracker) executeWriteSimple(req WriteRequest) error {
	ut.writeMu.Lock()
	defer ut.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(req.Context, 30*time.Second)
	defer cancel()

	// 直接执行，不使用事务（对于简单INSERT/UPDATE，不一定需要事务）
	if req.EventType == "vacuum" {
		// VACUUM不能在事务中执行
		_, err := ut.writeDB.ExecContext(ctx, req.Query, req.Args...)
		return err
	}

	// 其他操作使用短事务
	tx, err := ut.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				slog.Debug("Failed to rollback transaction", "error", rbErr, "event_type", req.EventType)
			}
		}
	}()

	result, err := tx.ExecContext(ctx, req.Query, req.Args...)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}

	// 对于必须命中已存在记录的事件类型，确保更新确实发生，避免“看似成功但数据未写入”
	if req.EventType == "update" && result != nil {
		if rows, err := result.RowsAffected(); err == nil && rows == 0 {
			return fmt.Errorf("no rows updated for event_type=update")
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	committed = true
	return nil
}

// executeWrite 执行单个写操作
func (ut *UsageTracker) executeWrite(req WriteRequest) error {
	ut.writeMu.Lock()
	defer ut.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(req.Context, 60*time.Second)
	defer cancel()

	// 安全的事务处理
	return ut.executeWriteTransaction(ctx, req)
}

// executeWriteTransaction 执行写事务（修复defer tx.Rollback()问题）
func (ut *UsageTracker) executeWriteTransaction(ctx context.Context, req WriteRequest) error {
	tx, err := ut.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				slog.Error("Failed to rollback transaction", "error", rbErr, "event_type", req.EventType)
			}
		}
	}()

	_, err = tx.ExecContext(ctx, req.Query, req.Args...)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	committed = true // 标记已提交，避免重复Rollback
	return nil
}

// initDatabaseWithWriteDB 使用写连接初始化数据库（同步等待完成）
func (ut *UsageTracker) initDatabaseWithWriteDB() error {
	// 读取并执行 schema SQL
	schemaSQL, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("failed to read schema.sql: %w", err)
	}

	// 使用写连接直接执行 schema（同步方式，确保表创建完成）
	if _, err := ut.writeDB.Exec(string(schemaSQL)); err != nil {
		return fmt.Errorf("failed to execute schema: %w", err)
	}

	slog.Debug("Database schema initialized successfully with write connection")
	return nil
}

// ============================================================================
// 🔥 v4.1 热池相关方法
// ============================================================================

// GetActiveRequests 获取热池中的活跃请求
// 返回当前正在处理中的请求列表（内存中）
func (ut *UsageTracker) GetActiveRequests() []*ActiveRequest {
	if ut.hotPool == nil {
		return nil
	}
	return ut.hotPool.GetActive()
}

// GetActiveRequestCount 获取活跃请求数量
func (ut *UsageTracker) GetActiveRequestCount() int {
	if ut.hotPool == nil {
		return 0
	}
	return ut.hotPool.GetActiveCount()
}

// GetHotPoolStats 获取热池统计信息
func (ut *UsageTracker) GetHotPoolStats() *HotPoolStats {
	if ut.hotPool == nil {
		return nil
	}
	stats := ut.hotPool.GetStats()
	return &stats
}

// GetArchiveStats 获取归档管理器统计信息
func (ut *UsageTracker) GetArchiveStats() *ArchiveStats {
	if ut.archiveManager == nil {
		return nil
	}
	stats := ut.archiveManager.GetStats()
	return &stats
}

// IsHotPoolEnabled 检查热池模式是否启用
func (ut *UsageTracker) IsHotPoolEnabled() bool {
	return ut.hotPoolEnabled
}

// GetActiveRequest 获取单个活跃请求
func (ut *UsageTracker) GetActiveRequest(requestID string) (*ActiveRequest, bool) {
	if ut.hotPool == nil {
		return nil, false
	}
	return ut.hotPool.Get(requestID)
}

// IsRequestActive 检查请求是否在热池中
func (ut *UsageTracker) IsRequestActive(requestID string) bool {
	if ut.hotPool == nil {
		return false
	}
	return ut.hotPool.Exists(requestID)
}

// UpdateActiveRequestTokens 更新活跃请求的Token（流式请求累积）
func (ut *UsageTracker) UpdateActiveRequestTokens(requestID string, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64) error {
	if ut.hotPool == nil {
		return fmt.Errorf("hot pool not enabled")
	}

	return ut.hotPool.Update(requestID, func(req *ActiveRequest) {
		req.InputTokens = inputTokens
		req.OutputTokens = outputTokens
		req.CacheCreationTokens = cacheCreationTokens
		req.CacheReadTokens = cacheReadTokens
	})
}

// GetHotPoolAndArchiveOverview 获取热池和归档概览（用于监控）
func (ut *UsageTracker) GetHotPoolAndArchiveOverview() map[string]interface{} {
	overview := map[string]interface{}{
		"hot_pool_enabled": ut.hotPoolEnabled,
	}

	if ut.hotPool != nil {
		stats := ut.hotPool.GetStats()
		overview["hot_pool"] = map[string]interface{}{
			"current_size":   stats.CurrentSize,
			"peak_size":      stats.PeakSize,
			"total_added":    stats.TotalAdded,
			"total_removed":  stats.TotalRemoved,
			"total_archived": stats.TotalArchived,
			"total_expired":  stats.TotalExpired,
			"total_overflow": stats.TotalOverflow,
		}
	}

	if ut.archiveManager != nil {
		stats := ut.archiveManager.GetStats()
		overview["archive_manager"] = map[string]interface{}{
			"total_received":     stats.TotalReceived,
			"total_archived":     stats.TotalArchived,
			"total_failed":       stats.TotalFailed,
			"total_dropped":      stats.TotalDropped,
			"total_batches":      stats.TotalBatches,
			"avg_batch_size":     stats.AvgBatchSize,
			"channel_length":     stats.ChannelLength,
			"last_flush_time":    stats.LastFlushTime,
			"last_flush_latency": stats.LastFlushLatency.String(),
		}
	}

	return overview
}

// ============================================================================
// 🔥 v4.1 双源查询方法（热池 + 数据库）
// ============================================================================

// ActiveRequestToDetail 将 ActiveRequest 转换为 RequestDetail
func (ut *UsageTracker) ActiveRequestToDetail(req *ActiveRequest) RequestDetail {
	var endTime *time.Time
	var durationMs *int64
	var httpStatus *int

	if req.EndTime != nil {
		endTime = req.EndTime
	}
	if req.DurationMs > 0 {
		durationMs = &req.DurationMs
	}
	if req.HTTPStatus > 0 {
		httpStatus = &req.HTTPStatus
	}

	// 计算成本（使用公共函数消除重复代码）
	var cost CostBreakdown
	if ut.pricing != nil {
		pricing, exists := ut.pricing[req.ModelName]
		if !exists {
			pricing, exists = ut.pricing["_default"]
		}
		if exists {
			// 获取端点倍率
			var multiplier *EndpointMultiplier
			if ut.endpointMu != nil {
				if m, ok := ut.endpointMu[req.EndpointName]; ok {
					multiplier = &m
				}
			}

			// 调用公共成本计算函数
			// TODO: 需要从请求中识别缓存类型（5分钟 vs 1小时），暂时默认使用 5 分钟倍率
			cost = CalculateCost(
				req.InputTokens, req.OutputTokens, req.CacheCreationTokens, req.CacheReadTokens,
				&pricing, multiplier, false,
			)
		}
	}

	return RequestDetail{
		ID:                    0, // 热池中的请求还没有数据库ID
		RequestID:             req.RequestID,
		ClientIP:              req.ClientIP,
		UserAgent:             req.UserAgent,
		Method:                req.Method,
		Path:                  req.Path,
		StartTime:             req.StartTime,
		EndTime:               endTime,
		DurationMs:            durationMs,
		EndpointName:          req.EndpointName,
		Channel:               req.Channel, // v5.0: 渠道标签
		GroupName:             req.GroupName,
		ModelName:             req.ModelName,
		AuthType:              req.AuthType,
		AuthKey:               req.AuthKey,
		IsStreaming:           req.IsStreaming,
		Status:                req.Status,
		HTTPStatusCode:        httpStatus,
		RetryCount:            req.RetryCount,
		FailureReason:         req.FailureReason,
		LastFailureReason:     "",
		CancelReason:          req.CancelReason,
		InputTokens:           req.InputTokens,
		OutputTokens:          req.OutputTokens,
		CacheCreationTokens:   req.CacheCreationTokens,
		CacheCreation5mTokens: req.CacheCreation5mTokens, // v5.0.1+
		CacheCreation1hTokens: req.CacheCreation1hTokens, // v5.0.1+
		CacheReadTokens:       req.CacheReadTokens,
		InputCostUSD:          cost.InputCost,
		OutputCostUSD:         cost.OutputCost,
		CacheCreationCostUSD:  cost.CacheCreationCost,
		CacheReadCostUSD:      cost.CacheReadCost,
		TotalCostUSD:          cost.TotalCost,
		CreatedAt:             req.StartTime,
		UpdatedAt:             ut.now(),
	}
}

// QueryRequestDetailsWithHotPool 双源查询：热池 + 数据库
// 返回合并后的请求列表，热池中的活跃请求排在前面
// 🔧 [修复] 2025-12-11: 正确处理分页，确保返回数量不超过 limit
func (ut *UsageTracker) QueryRequestDetailsWithHotPool(ctx context.Context, opts *QueryOptions) ([]RequestDetail, int64, error) {
	var results []RequestDetail

	// 获取分页参数
	limit := opts.Limit
	offset := opts.Offset
	if limit <= 0 {
		limit = 20 // 默认每页20条
	}

	// 1. 从热池获取所有符合过滤条件的活跃请求（用于计算总数和分页）
	allHotPoolRequests := ut.getFilteredHotPoolRequests(opts)
	hotPoolCount := len(allHotPoolRequests)

	// 2. 获取数据库总数（用于分页计算）
	dbCount, err := ut.CountRequestDetails(ctx, opts)
	if err != nil {
		slog.Warn("Failed to count database requests", "error", err)
		dbCount = 0
	}

	// 3. 计算总数：热池 + 数据库
	totalCount := int64(hotPoolCount) + int64(dbCount)

	// 4. 热池数据按开始时间倒序排列（最新的在前）
	if hotPoolCount > 0 {
		sort.Slice(allHotPoolRequests, func(i, j int) bool {
			return allHotPoolRequests[i].StartTime.After(allHotPoolRequests[j].StartTime)
		})
	}

	// 创建热池请求ID集合用于去重
	hotPoolIDs := make(map[string]bool)
	for _, req := range allHotPoolRequests {
		hotPoolIDs[req.RequestID] = true
	}

	// 5. 根据 offset 和 limit 决定从哪里取数据
	if offset < hotPoolCount {
		// 当前页包含热池数据
		hotPoolEnd := offset + limit
		if hotPoolEnd > hotPoolCount {
			hotPoolEnd = hotPoolCount
		}
		// 从热池取数据
		results = append(results, allHotPoolRequests[offset:hotPoolEnd]...)

		// 如果热池数据不够一页，从数据库补充
		remaining := limit - len(results)
		if remaining > 0 {
			// 从数据库查询，offset=0 因为热池数据已经占据了前面的位置
			dbOpts := *opts
			dbOpts.Offset = 0
			dbOpts.Limit = remaining + hotPoolCount // 多取一些用于去重
			dbRequests, err := ut.QueryRequestDetails(ctx, &dbOpts)
			if err != nil {
				slog.Warn("Failed to query database requests", "error", err)
			} else {
				// 添加数据库请求（排除已在热池中的）
				for _, req := range dbRequests {
					if !hotPoolIDs[req.RequestID] && len(results) < limit {
						results = append(results, req)
					}
				}
			}
		}
	} else {
		// 当前页只有数据库数据
		// 调整数据库查询的 offset（减去热池数据的数量）
		dbOpts := *opts
		dbOpts.Offset = offset - hotPoolCount
		dbOpts.Limit = limit + hotPoolCount // 多取一些用于去重

		dbRequests, err := ut.QueryRequestDetails(ctx, &dbOpts)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to query database: %w", err)
		}

		// 添加数据库请求（排除已在热池中的）
		for _, req := range dbRequests {
			if !hotPoolIDs[req.RequestID] && len(results) < limit {
				results = append(results, req)
			}
		}
	}

	return results, totalCount, nil
}

// getFilteredHotPoolRequests 获取过滤后的热池请求
func (ut *UsageTracker) getFilteredHotPoolRequests(opts *QueryOptions) []RequestDetail {
	if ut.hotPool == nil {
		return nil
	}

	activeRequests := ut.hotPool.GetActive()
	if len(activeRequests) == 0 {
		return nil
	}

	var filtered []RequestDetail
	for _, req := range activeRequests {
		// 应用过滤条件
		if opts != nil {
			// 状态过滤
			if opts.Status != "" && req.Status != opts.Status {
				continue
			}
			// 模型过滤
			if opts.ModelName != "" && req.ModelName != opts.ModelName {
				continue
			}
			// 渠道过滤（v5.0）
			if opts.Channel != "" && req.Channel != opts.Channel {
				continue
			}
			// 端点过滤
			if opts.EndpointName != "" && req.EndpointName != opts.EndpointName {
				continue
			}
			// 组过滤
			if opts.GroupName != "" && req.GroupName != opts.GroupName {
				continue
			}
			// 时间范围过滤
			if opts.StartDate != nil && req.StartTime.Before(*opts.StartDate) {
				continue
			}
			if opts.EndDate != nil && req.StartTime.After(*opts.EndDate) {
				continue
			}
		}

		filtered = append(filtered, ut.ActiveRequestToDetail(req))
	}

	return filtered
}

// GetHotPoolRequestCount 获取热池中符合条件的请求数量
func (ut *UsageTracker) GetHotPoolRequestCount(opts *QueryOptions) int {
	filtered := ut.getFilteredHotPoolRequests(opts)
	return len(filtered)
}
