// app.go - Wails 应用核心结构
// 封装所有业务组件，提供生命周期管理

package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cc-forwarder/config"
	"cc-forwarder/internal/endpoint"
	"cc-forwarder/internal/events"
	"cc-forwarder/internal/logging"
	"cc-forwarder/internal/middleware"
	"cc-forwarder/internal/proxy"
	"cc-forwarder/internal/service"
	"cc-forwarder/internal/store"
	"cc-forwarder/internal/tracking"
	"cc-forwarder/internal/transport"
	"cc-forwarder/internal/utils"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App 是 Wails 应用的核心结构
// 它封装了所有业务组件，并暴露方法给前端调用
type App struct {
	// Wails 上下文
	ctx context.Context

	// 核心组件
	config               *config.Config
	configWatcher        *config.ConfigWatcher
	logger               *slog.Logger
	endpointManager      *endpoint.Manager
	eventBus             events.EventBus // 接口类型，不是指针
	usageTracker         *tracking.UsageTracker
	storeDB              *sql.DB // v6.1+: 管理/配置专用 DB 连接，避免被 tracking 写入阻塞
	proxyHandler         *proxy.Handler
	loggingMiddleware    *middleware.LoggingMiddleware
	monitoringMiddleware *middleware.MonitoringMiddleware
	authMiddleware       *middleware.AuthMiddleware

	// v5.0+ 端点存储 (SQLite)
	endpointStore   store.EndpointStore      // 端点数据持久化
	endpointService *service.EndpointService // 端点业务服务

	// v6.1+ 渠道存储 (SQLite)
	channelStore   store.ChannelStore      // 渠道数据持久化
	channelService *service.ChannelService // 渠道业务服务

	// v5.0+ 模型定价存储 (SQLite)
	modelPricingStore   store.ModelPricingStore      // 模型定价数据持久化
	modelPricingService *service.ModelPricingService // 模型定价业务服务

	// v5.1+ 系统设置存储 (SQLite)
	settingsStore   store.SettingsStore      // 设置数据持久化
	settingsService *service.SettingsService // 设置业务服务
	portManager     *utils.PortManager       // 端口管理器

	// HTTP 代理服务器 (保留，监听配置的端口)
	proxyServer *http.Server

	// 应用状态
	startTime  time.Time
	configPath string

	// 并发控制
	mu        sync.RWMutex
	isRunning bool
	quitting  int32

	// 日志处理器（用于查询和广播）
	logHandler *logging.BroadcastHandler
	logEmitter *logging.EventEmitter
}

// NewApp 创建新的应用实例
func NewApp() *App {
	return &App{
		startTime: time.Now(),
	}
}

// startup 在 Wails 应用启动时调用
// 这里初始化所有组件并启动代理服务器
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// 1. 加载配置
	a.loadConfig()

	// 2. 初始化日志
	a.setupLogger()

	// 3. 显示启动信息
	a.logger.Info("🚀 CC-Forwarder 桌面版启动中...",
		"version", Version,
		"config_file", a.configPath)

	// 4. 初始化事件总线
	a.setupEventBus()

	// 5. 初始化使用追踪（SQLite 存储需要依赖数据库）
	a.setupUsageTracker()

	// 5.2 初始化管理/配置 DB（channels/endpoints/settings 等）
	a.setupStoreDB()

	// 5.5 初始化设置服务 (v5.1+ SQLite)
	a.setupSettingsStore()

	// 6. 创建端点管理器（但不启动健康检查）
	a.endpointManager = endpoint.NewManager(a.config)
	a.endpointManager.SetEventBus(a.eventBus)
	// v5.0+ Wails 桌面应用：设置健康检查完成回调，推送事件到前端
	a.endpointManager.SetOnHealthCheckComplete(func() {
		a.emitEndpointUpdate()
	})

	// v6.0+ 设置故障转移回调：以“渠道(channel)”为单位同步数据库状态
	a.endpointManager.SetOnFailoverTriggered(func(failedChannel, newChannel string) {
		if a.endpointService != nil {
			ctx := context.Background()
			if newChannel != "" {
				if err := a.endpointService.ActivateChannel(ctx, newChannel); err != nil {
					slog.Warn(fmt.Sprintf("⚠️ [故障转移回调] 激活新渠道失败: %s - %v", newChannel, err))
				}
			}
			slog.Info(fmt.Sprintf("✅ [故障转移回调] 渠道已切换并同步数据库: %s → %s", failedChannel, newChannel))
		}

		// 推送事件到前端
		a.emitEndpointUpdate()
	})

	// 7. 初始化端点存储 (v5.0+ SQLite, 需要在创建 Manager 之后)
	// 从数据库同步端点到 Manager
	if a.config.EndpointsStorage.Type == "sqlite" {
		a.setupEndpointStore()
	}

	// 7.5 初始化模型定价存储 (v5.0+ SQLite)
	a.setupModelPricingStore()

	// 7.6 同步端点倍率到 UsageTracker（用于成本计算）
	a.syncEndpointMultipliersToTracker(ctx)

	// 8. 启动端点管理器（此时端点已从数据库加载完成）
	a.endpointManager.Start()

	// 显示代理配置
	if a.config.Proxy.Enabled {
		proxyInfo := transport.GetProxyInfo(a.config)
		a.logger.Info("🔗 " + proxyInfo)
	}

	// 9. 初始化代理处理器
	a.setupProxyHandler()

	// 10. 启动 HTTP 代理服务器
	a.startProxyServer()

	// 11. 设置配置热重载
	a.setupConfigReload()

	// 12. 设置事件桥接
	a.setupEventBridges()

	// 13. 启动历史数据收集器
	a.startHistoryCollector()

	a.isRunning = true
	a.logger.Info("✅ CC-Forwarder 启动完成",
		"proxy_port", a.config.Server.Port)
}

// shutdown 在 Wails 应用关闭时调用
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	logger := a.logger
	proxyServer := a.proxyServer
	usageTracker := a.usageTracker
	storeDB := a.storeDB
	endpointManager := a.endpointManager
	eventBus := a.eventBus
	configWatcher := a.configWatcher
	logEmitter := a.logEmitter
	a.isRunning = false
	a.mu.Unlock()

	if logger != nil {
		logger.Info("🛑 正在关闭 CC-Forwarder...")
	}

	// 1. 停止接收新请求
	if proxyServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := proxyServer.Shutdown(shutdownCtx); err != nil {
			_ = proxyServer.Close()
			if logger != nil {
				logger.Error("代理服务器关闭失败", "error", err)
			}
		}
	}

	// 2. 关闭使用追踪 (flush 数据库)
	if usageTracker != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := usageTracker.Close(); err != nil {
				if logger != nil {
					logger.Error("使用追踪器关闭失败", "error", err)
				}
			}
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if logger != nil {
				logger.Warn("使用追踪器关闭超时，强制继续退出")
			}
		}
	}

	// 2.1 关闭管理/配置 DB
	if storeDB != nil {
		if err := storeDB.Close(); err != nil {
			if logger != nil {
				logger.Error("管理数据库关闭失败", "error", err)
			}
		}
	}

	// 3. 关闭端点管理器
	if endpointManager != nil {
		endpointManager.Stop()
	}

	// 4. 关闭事件总线
	if eventBus != nil {
		if err := eventBus.Stop(); err != nil {
			if logger != nil {
				logger.Error("事件总线关闭失败", "error", err)
			}
		}
	}

	// 5. 关闭配置监听
	if configWatcher != nil {
		_ = configWatcher.Close()
	}

	// 6. 停止日志事件发射器
	if logEmitter != nil {
		logEmitter.Stop()
	}

	a.mu.Lock()
	a.proxyServer = nil
	a.usageTracker = nil
	a.storeDB = nil
	a.mu.Unlock()

	if logger != nil {
		logger.Info("✅ CC-Forwarder 已关闭")
	}
}

// domReady 在前端 DOM 准备就绪时调用
func (a *App) domReady(ctx context.Context) {
	// 发送初始状态给前端
	a.emitSystemStatus()
}

// beforeClose 在窗口关闭前调用，返回 true 阻止关闭
func (a *App) beforeClose(ctx context.Context) bool {
	// Windows 下用户“关闭窗口”应直接退出进程，避免遗留后台代理服务进程。
	if !atomic.CompareAndSwapInt32(&a.quitting, 0, 1) {
		return false
	}

	// 主动触发应用退出；返回 true 阻止默认关闭流程（由 Quit 统一收口到 OnShutdown）。
	// 注意：Quit 可能触发同步回调，避免在 BeforeClose 回调里阻塞 UI 线程。
	go runtime.Quit(ctx)
	return true
}

// loadConfig 加载配置
func (a *App) loadConfig() {
	// 创建临时 logger 用于初始化
	tempLogger := slog.Default()

	// 确保应用目录存在
	if err := utils.EnsureAppDirs(); err != nil {
		tempLogger.Warn("⚠️ 无法创建应用目录", "error", err)
	} else {
		tempLogger.Info("📁 应用目录已就绪",
			"appdir", utils.GetAppDataDir(),
			"data", utils.GetDataDir(),
			"logs", utils.GetLogDir())
	}

	// 直接从嵌入的配置加载（不写文件）
	tempLogger.Info("📝 从嵌入配置加载")

	// 将嵌入的配置写入临时文件进行解析
	tmpConfigPath := filepath.Join(os.TempDir(), "cc-forwarder-config.yaml")

	// 先修改配置内容，替换路径为用户目录
	configContent := string(defaultConfigContent)
	// 注意：不修改配置内容，而是加载后再覆盖路径

	if err := os.WriteFile(tmpConfigPath, []byte(configContent), 0644); err != nil {
		panic(fmt.Sprintf("无法创建临时配置文件: %v", err))
	}
	defer os.Remove(tmpConfigPath)

	// 创建配置监听器（此时会调用 SetDefaults 设置默认路径）
	configWatcher, err := config.NewConfigWatcher(tmpConfigPath, tempLogger)
	if err != nil {
		panic(fmt.Sprintf("无法加载配置: %v", err))
	}

	a.configWatcher = configWatcher
	cfg := configWatcher.GetConfig()

	// ⚠️ 关键：立即覆盖所有路径为用户目录（在任何组件初始化之前）
	cfg.Logging.FilePath = filepath.Join(utils.GetLogDir(), "app.log")
	// v6.1+：为避免与旧版本共享同一 usage.db 产生锁冲突，默认切换到新文件名，并在启动时自动迁移旧数据。
	cfg.UsageTracking.DatabasePath = filepath.Join(utils.GetDataDir(), "cc-forwarder.db")

	// 同时设置 Database 配置（如果存在）
	if cfg.UsageTracking.Database != nil {
		// Database 配置中没有 DatabasePath 字段，UsageTracking.DatabasePath 是统一路径
	}

	a.config = cfg

	tempLogger.Info("✅ 配置加载完成",
		"log_path", a.config.Logging.FilePath,
		"db_path", a.config.UsageTracking.DatabasePath,
		"appdir", utils.GetAppDataDir())

	a.configPath = tmpConfigPath
}

// setupLogger 设置日志
func (a *App) setupLogger() {
	logger, broadcastHandler := setupLogger(a.config.Logging)
	a.logger = logger
	slog.SetDefault(logger)

	// 存储日志处理器和发射器引用
	a.logHandler = broadcastHandler
	a.logEmitter = broadcastHandler.Emitter

	// 初始化调试配置
	utils.SetDebugConfig(a.config)

	a.logger.Info("✅ 日志系统初始化完成",
		"level", a.config.Logging.Level,
		"file_enabled", a.config.Logging.FileEnabled)
}

// setupEventBus 设置事件总线
func (a *App) setupEventBus() {
	a.eventBus = events.NewEventBus(a.logger)
	if err := a.eventBus.Start(); err != nil {
		a.logger.Error("事件总线启动失败", "error", err)
	}
}

// setupEndpointStore 设置端点存储 (v5.0+ SQLite)
func (a *App) setupEndpointStore() {
	// 获取数据库连接：优先使用 storeDB（避免 tracking 写入阻塞管理写操作）。
	// 注意：端点/渠道管理属于“管理存储”，不应强耦合到 usage tracking 是否启用。
	db := a.storeDB
	if db == nil && a.usageTracker != nil {
		db = a.usageTracker.GetDB()
	}
	if db == nil {
		a.logger.Error("❌ 无法获取数据库连接")
		return
	}

	// 创建 EndpointStore
	a.endpointStore = store.NewSQLiteEndpointStore(db)

	// 创建 EndpointService
	a.endpointService = service.NewEndpointService(a.endpointStore, a.endpointManager, a.config)

	// 创建 ChannelStore / ChannelService
	a.channelStore = store.NewSQLiteChannelStore(db)
	a.channelService = service.NewChannelService(a.channelStore)

	// 从数据库同步端点到内存
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.endpointService.SyncFromDatabase(ctx); err != nil {
		a.logger.Warn("⚠️ 从数据库同步端点失败，使用 YAML 配置", "error", err)
	} else {
		// v6.1+: 回填渠道表，保证“端点删光后渠道仍存在”
		if a.channelService != nil {
			if added, err := a.channelService.BackfillChannelsFromEndpoints(ctx, a.endpointStore); err != nil {
				a.logger.Warn("⚠️ 渠道回填失败", "error", err)
			} else if added > 0 {
				a.logger.Info("✅ 渠道回填完成", "count", added)
			}
		}

		// v6.1+: 同步渠道优先级到运行时，用于渠道间故障转移顺序
		a.syncChannelPrioritiesToEndpointManager(ctx)
		a.logger.Info("✅ 端点存储已启用 (SQLite)")
	}
}

// setupModelPricingStore 设置模型定价存储 (v5.0+ SQLite)
func (a *App) setupModelPricingStore() {
	a.ensureModelPricingService()
	if a.modelPricingService == nil {
		if a.logger != nil {
			a.logger.Debug("模型定价存储跳过初始化 (数据库未就绪)")
		}
		return
	}

	// 检查是否需要初始化默认数据
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	count, err := a.modelPricingService.GetPricingCount(ctx)
	if err != nil {
		a.logger.Warn("⚠️ 检查模型定价数据失败", "error", err)
		return
	}

	// 如果表为空，初始化默认数据
	if count == 0 {
		a.initDefaultModelPricing(ctx)
	}

	// 加载缓存
	if err := a.modelPricingService.LoadCache(ctx); err != nil {
		a.logger.Warn("⚠️ 加载模型定价缓存失败", "error", err)
	}

	// 同步定价到 UsageTracker（用于成本计算）
	a.syncPricingToTracker(ctx)

	a.logger.Info("✅ 模型定价存储已启用 (SQLite)", "count", count)
}

// ensureModelPricingService 确保模型定价服务已初始化。
// 目的：避免因启动顺序/初始化失败导致前端提示“未启用”，同时不把模型定价强耦合到 UsageTracker 的生命周期。
func (a *App) ensureModelPricingService() {
	a.mu.RLock()
	if a.modelPricingService != nil {
		a.mu.RUnlock()
		return
	}
	db := a.storeDB
	if db == nil && a.usageTracker != nil {
		db = a.usageTracker.GetDB()
	}
	a.mu.RUnlock()

	if db == nil {
		return
	}

	modelPricingStore := store.NewSQLiteModelPricingStore(db)
	modelPricingService := service.NewModelPricingService(modelPricingStore)

	a.mu.Lock()
	if a.modelPricingService == nil {
		a.modelPricingStore = modelPricingStore
		a.modelPricingService = modelPricingService
	}
	a.mu.Unlock()
}

// initDefaultModelPricing 初始化默认模型定价数据
func (a *App) initDefaultModelPricing(ctx context.Context) {
	// Claude 官方定价 (2025年最新)
	// 5分钟缓存: input * 1.25, 1小时缓存: input * 2.0, 读取: input * 0.1
	defaultPricings := []*store.ModelPricingRecord{
		// 默认定价
		{
			ModelName:            "_default",
			DisplayName:          "默认定价",
			Description:          "未知模型使用的默认定价",
			InputPrice:           3.0,
			OutputPrice:          15.0,
			CacheCreationPrice5m: 3.75, // 3.0 * 1.25
			CacheCreationPrice1h: 6.0,  // 3.0 * 2.0
			CacheReadPrice:       0.30, // 3.0 * 0.1
			IsDefault:            true,
		},
		// Claude Sonnet 4
		{
			ModelName:            "claude-sonnet-4-20250514",
			DisplayName:          "Claude Sonnet 4",
			Description:          "Claude Sonnet 4 (2025-05-14)",
			InputPrice:           3.0,
			OutputPrice:          15.0,
			CacheCreationPrice5m: 3.75,
			CacheCreationPrice1h: 6.0,
			CacheReadPrice:       0.30,
		},
		// Claude 3.5 Sonnet
		{
			ModelName:            "claude-3-5-sonnet-20241022",
			DisplayName:          "Claude 3.5 Sonnet",
			Description:          "Claude 3.5 Sonnet (2024-10-22)",
			InputPrice:           3.0,
			OutputPrice:          15.0,
			CacheCreationPrice5m: 3.75,
			CacheCreationPrice1h: 6.0,
			CacheReadPrice:       0.30,
		},
		// Claude 3.5 Haiku
		{
			ModelName:            "claude-3-5-haiku-20241022",
			DisplayName:          "Claude 3.5 Haiku",
			Description:          "Claude 3.5 Haiku (2024-10-22)",
			InputPrice:           0.80,
			OutputPrice:          4.0,
			CacheCreationPrice5m: 1.0,  // 0.80 * 1.25
			CacheCreationPrice1h: 1.6,  // 0.80 * 2.0
			CacheReadPrice:       0.08, // 0.80 * 0.1
		},
		// Claude Opus 4
		{
			ModelName:            "claude-opus-4-20250514",
			DisplayName:          "Claude Opus 4",
			Description:          "Claude Opus 4 (2025-05-14)",
			InputPrice:           15.0,
			OutputPrice:          75.0,
			CacheCreationPrice5m: 18.75, // 15.0 * 1.25
			CacheCreationPrice1h: 30.0,  // 15.0 * 2.0
			CacheReadPrice:       1.50,  // 15.0 * 0.1
		},
		// ========== Claude 4.5 系列 (2025年最新) ==========
		// Claude Sonnet 4.5
		{
			ModelName:            "claude-sonnet-4-5-20250929",
			DisplayName:          "Claude Sonnet 4.5",
			Description:          "Claude Sonnet 4.5 (2025-09-29)",
			InputPrice:           3.0,
			OutputPrice:          15.0,
			CacheCreationPrice5m: 3.75, // 3.0 * 1.25
			CacheCreationPrice1h: 6.0,  // 3.0 * 2.0
			CacheReadPrice:       0.30, // 3.0 * 0.1
		},
		// Claude Haiku 4.5
		{
			ModelName:            "claude-haiku-4-5-20251001",
			DisplayName:          "Claude Haiku 4.5",
			Description:          "Claude Haiku 4.5 (2025-10-01)",
			InputPrice:           1.0,
			OutputPrice:          5.0,
			CacheCreationPrice5m: 1.25, // 1.0 * 1.25
			CacheCreationPrice1h: 2.0,  // 1.0 * 2.0
			CacheReadPrice:       0.10, // 1.0 * 0.1
		},
		// Claude Opus 4.5
		{
			ModelName:            "claude-opus-4-5-20251101",
			DisplayName:          "Claude Opus 4.5",
			Description:          "Claude Opus 4.5 (2025-11-01)",
			InputPrice:           5.0,
			OutputPrice:          25.0,
			CacheCreationPrice5m: 6.25, // 5.0 * 1.25
			CacheCreationPrice1h: 10.0, // 5.0 * 2.0
			CacheReadPrice:       0.50, // 5.0 * 0.1
		},
		// ========== 旧版本兼容 ==========
		{
			ModelName:            "claude-3-opus-20240229",
			DisplayName:          "Claude 3 Opus",
			Description:          "Claude 3 Opus (2024-02-29)",
			InputPrice:           15.0,
			OutputPrice:          75.0,
			CacheCreationPrice5m: 18.75,
			CacheCreationPrice1h: 30.0,
			CacheReadPrice:       1.50,
		},
		{
			ModelName:            "claude-3-sonnet-20240229",
			DisplayName:          "Claude 3 Sonnet",
			Description:          "Claude 3 Sonnet (2024-02-29)",
			InputPrice:           3.0,
			OutputPrice:          15.0,
			CacheCreationPrice5m: 3.75,
			CacheCreationPrice1h: 6.0,
			CacheReadPrice:       0.30,
		},
		{
			ModelName:            "claude-3-haiku-20240307",
			DisplayName:          "Claude 3 Haiku",
			Description:          "Claude 3 Haiku (2024-03-07)",
			InputPrice:           0.25,
			OutputPrice:          1.25,
			CacheCreationPrice5m: 0.31,  // 0.25 * 1.25
			CacheCreationPrice1h: 0.50,  // 0.25 * 2.0
			CacheReadPrice:       0.025, // 0.25 * 0.1
		},
	}

	if err := a.modelPricingStore.BatchUpsert(ctx, defaultPricings); err != nil {
		a.logger.Error("❌ 初始化默认模型定价失败", "error", err)
		return
	}

	a.logger.Info("✅ 已初始化默认模型定价", "count", len(defaultPricings))
}

// syncPricingToTracker 同步模型定价到 UsageTracker
func (a *App) syncPricingToTracker(ctx context.Context) {
	if a.usageTracker == nil || a.modelPricingService == nil {
		return
	}

	records, err := a.modelPricingService.ListPricings(ctx)
	if err != nil {
		a.logger.Warn("⚠️ 获取模型定价列表失败", "error", err)
		return
	}

	// 转换为 tracking.ModelPricing 格式
	pricing := make(map[string]tracking.ModelPricing)
	for _, r := range records {
		pricing[r.ModelName] = a.modelPricingService.ToTrackingPricing(r)
	}

	// 更新 UsageTracker 的定价缓存
	a.usageTracker.UpdatePricing(pricing)
	a.logger.Debug("已同步模型定价到 UsageTracker", "count", len(pricing))
}

// syncEndpointMultipliersToTracker 同步端点倍率到 UsageTracker
// 成本计算公式：模型基础定价 * 端点倍率
func (a *App) syncEndpointMultipliersToTracker(ctx context.Context) {
	if a.usageTracker == nil || a.endpointStore == nil {
		return
	}

	endpoints, err := a.endpointStore.List(ctx)
	if err != nil {
		a.logger.Warn("⚠️ 获取端点列表失败", "error", err)
		return
	}

	// 转换为 tracking.EndpointMultiplier 格式
	multipliers := make(map[string]tracking.EndpointMultiplier)
	for _, ep := range endpoints {
		key := tracking.EndpointMultiplierKey(ep.Channel, ep.Name)
		multipliers[key] = tracking.EndpointMultiplier{
			CostMultiplier:                ep.CostMultiplier,
			InputCostMultiplier:           ep.InputCostMultiplier,
			OutputCostMultiplier:          ep.OutputCostMultiplier,
			CacheCreationCostMultiplier:   ep.CacheCreationCostMultiplier,
			CacheCreationCostMultiplier1h: ep.CacheCreationCostMultiplier1h,
			CacheReadCostMultiplier:       ep.CacheReadCostMultiplier,
		}
	}

	// 更新 UsageTracker 的端点倍率缓存
	a.usageTracker.UpdateEndpointMultipliers(multipliers)
	a.logger.Debug("已同步端点倍率到 UsageTracker", "count", len(multipliers))
}

// getEffectiveUsageDBPath returns the single SQLite database path used by:
// - usage tracker (request_logs / usage_summary / ...)
// - management stores (channels/endpoints/settings/model_pricing)
//
// 统一数据库路径：内部只使用 UsageTracking.DatabasePath（由 config.SetDefaults 将 database.path 归一到该字段）。
func (a *App) getEffectiveUsageDBPath() string {
	if a == nil || a.config == nil {
		return ""
	}
	if a.config.UsageTracking.DatabasePath != "" {
		return a.config.UsageTracking.DatabasePath
	}
	return filepath.Join(utils.GetDataDir(), "cc-forwarder.db")
}

// setupUsageTracker 设置使用追踪
func (a *App) setupUsageTracker() {
	if !a.config.UsageTracking.Enabled {
		a.logger.Info("📊 使用追踪已禁用")
		return
	}

	// 统一数据库路径（避免 Database.Path 为空时回退到工作目录，导致历史数据“看不到/分裂”）
	dbPath := a.getEffectiveUsageDBPath()
	a.config.UsageTracking.DatabasePath = dbPath

	// v6.1+ 自动迁移：如果新库不存在但旧库存在，则复制旧库到新路径。
	a.migrateLegacyDatabaseIfNeeded()

	a.logger.Info("📊 初始化使用追踪器", "db_path", dbPath)

	// v5.0+ 重构：定价配置完全从 SQLite 加载，不再依赖 config.yaml
	// 初始化时使用空定价，后续由 syncPricingToTracker() 从数据库加载
	trackingConfig := &tracking.Config{
		Enabled:         a.config.UsageTracking.Enabled,
		DatabasePath:    dbPath,
		Database:        a.config.UsageTracking.Database,
		BufferSize:      a.config.UsageTracking.BufferSize,
		BatchSize:       a.config.UsageTracking.BatchSize,
		FlushInterval:   a.config.UsageTracking.FlushInterval,
		MaxRetry:        a.config.UsageTracking.MaxRetry,
		RetentionDays:   a.config.UsageTracking.RetentionDays,
		CleanupInterval: a.config.UsageTracking.CleanupInterval,
		ModelPricing:    nil,                     // v5.0+: 定价从 SQLite model_pricing 表加载
		DefaultPricing:  tracking.ModelPricing{}, // v5.0+: 默认定价从 SQLite 加载
	}

	var err error
	a.usageTracker, err = tracking.NewUsageTracker(trackingConfig, a.config.Timezone)
	if err != nil {
		a.logger.Error("使用追踪器初始化失败", "error", err)
		return
	}

	a.logger.Info("📊 使用追踪已启用", "database", dbPath)
}

// setupStoreDB 初始化一个专用的 SQLite 连接用于管理写操作（channels/endpoints/settings）。
// 目的：避免 UsageTracker 的后台写入/归档在同一连接上阻塞 UI 管理操作，造成“前端超时但最终成功”的错觉。
func (a *App) setupStoreDB() {
	if a.config == nil {
		return
	}
	if a.storeDB != nil {
		return
	}
	dbPath := a.getEffectiveUsageDBPath()
	// 运行时回填，确保后续组件拿到统一路径（即使 usage_tracking.enabled=false）。
	a.config.UsageTracking.DatabasePath = dbPath

	// 即使未启用 usage tracking，也执行旧库迁移，避免共享 usage.db 导致锁冲突。
	a.migrateLegacyDatabaseIfNeeded()

	// 确保数据库目录存在
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			a.logger.Warn("⚠️ 创建数据库目录失败", "error", err)
			return
		}
	}

	// 使用较长 busy_timeout：管理侧读写应尽量避免 SQLITE_BUSY；上层仍用 ctx 控制总体等待时间。
	// 注意：写冲突时 SQLite 仍只有一个 writer，该超时只是让管理侧在短暂争用时更耐心等待。
	dsn := dbPath + "?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=10000&_foreign_keys=1&_busy_timeout=60000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		a.logger.Warn("⚠️ 初始化管理数据库连接失败", "error", err)
		return
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// 这里不做过短超时：如果用户正运行旧版本/另一实例占用数据库，短超时会导致 storeDB 为 nil，
	// 进而前端出现“设置服务未启用/数据为空”的连锁问题。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbReady := true
	if err := db.PingContext(ctx); err != nil {
		if isLikelySQLiteBusyOrLocked(err) || strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded") {
			dbReady = false
			a.logger.Warn("⚠️ 管理数据库连接暂不可用（可能被占用/初始化耗时较长），将继续启动并允许稍后重试", "error", err)
			a.emitNotification("warning", "数据库暂不可用", "检测到数据库可能被占用或初始化耗时较长；管理功能（设置/渠道/定价）可能暂不可用。请确保只运行一个 CC-Forwarder 实例，稍等后点击刷新，必要时重启。")
		} else {
			_ = db.Close()
			a.logger.Warn("⚠️ 管理数据库连接不可用", "error", err)
			return
		}
	}

	// 当 usageTracker 未启用/初始化失败时，仍需要确保管理表（settings/channels/endpoints/model_pricing 等）存在。
	if a.usageTracker == nil && dbReady {
		adapter, err := tracking.NewDatabaseAdapter(tracking.DatabaseConfig{
			Type:         "sqlite",
			DatabasePath: dbPath,
			Timezone:     a.config.Timezone,
		})
		if err == nil {
			if err := adapter.Open(); err == nil {
				if err := adapter.InitSchema(); err != nil {
					a.logger.Warn("⚠️ 初始化数据库 Schema 失败（管理存储）", "error", err)
				}
				_ = adapter.Close()
			} else {
				a.logger.Warn("⚠️ 打开数据库失败（管理存储）", "error", err)
			}
		} else {
			a.logger.Warn("⚠️ 创建数据库适配器失败（管理存储）", "error", err)
		}
	}

	a.storeDB = db
	a.logger.Info("✅ 管理数据库连接已就绪", "db", dbPath)
}

func isLikelySQLiteBusyOrLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "busy") ||
		strings.Contains(msg, "locked")
}

// migrateLegacyDatabaseIfNeeded 将旧版本默认库 usage.db 迁移到新版本默认库 cc-forwarder.db。
// 目的：避免新旧版本同时运行时共享同一 SQLite 文件导致锁冲突（创建渠道/写入配置“无响应”）。
func (a *App) migrateLegacyDatabaseIfNeeded() {
	newPath := a.getEffectiveUsageDBPath()
	if newPath == "" {
		return
	}

	// 仅对默认路径场景启用迁移，避免覆盖用户显式配置
	defaultNew := filepath.Join(utils.GetDataDir(), "cc-forwarder.db")
	defaultOld := filepath.Join(utils.GetDataDir(), "usage.db")
	if filepath.Clean(newPath) != filepath.Clean(defaultNew) {
		return
	}

	legacyCandidates := []string{}

	// 兼容旧版本/调试环境可能使用的相对路径（例如安装目录/data/*.db 或运行目录/data/*.db）
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		legacyCandidates = append(legacyCandidates,
			filepath.Join(exeDir, "data", "cc-forwarder.db"),
			filepath.Join(exeDir, "data", "usage.db"),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		legacyCandidates = append(legacyCandidates,
			filepath.Join(cwd, "data", "cc-forwarder.db"),
			filepath.Join(cwd, "data", "usage.db"),
		)
	}
	// 兼容更老版本：用户目录下的 usage.db
	legacyCandidates = append(legacyCandidates, defaultOld)

	existingLegacy := make([]string, 0, len(legacyCandidates))
	seen := make(map[string]struct{}, len(legacyCandidates))
	for _, p := range legacyCandidates {
		if p == "" {
			continue
		}
		cp := filepath.Clean(p)
		if cp == filepath.Clean(defaultNew) {
			continue
		}
		if _, ok := seen[cp]; ok {
			continue
		}
		if _, err := os.Stat(cp); err != nil {
			continue
		}
		seen[cp] = struct{}{}
		existingLegacy = append(existingLegacy, cp)
	}
	if len(existingLegacy) == 0 {
		return
	}

	// 场景1：新库不存在 -> 直接复制（最简单、最稳定）
	if _, err := os.Stat(defaultNew); err != nil {
		// 选择一个“最佳”的来源文件：优先 request_logs 数量更多的库（更可能包含用户的历史数据）。
		bestLegacy := existingLegacy[0]
		bestReq := int64(-1)
		bestCh := int64(-1)
		for _, p := range existingLegacy {
			reqCount, _ := countTableRows(p, "request_logs")
			chCount, _ := countTableRows(p, "channels")
			if reqCount > bestReq || (reqCount == bestReq && chCount > bestCh) {
				bestLegacy = p
				bestReq = reqCount
				bestCh = chCount
			}
		}

		if err := copyFile(bestLegacy, defaultNew); err != nil {
			a.logger.Warn("⚠️ 旧数据迁移失败，将使用新的空数据库（可在关闭旧版本后重启再迁移）",
				"old", bestLegacy, "new", defaultNew, "error", err)
			a.emitNotification("warning", "旧数据迁移失败", "无法复制旧数据库（可能被旧版本占用），将使用新的空数据库；关闭旧版本后重启可再次尝试迁移。")
			return
		}

		// 拷贝后确保 schema 完整（补表/补列/迁移），并从其他来源合并缺失数据。
		_ = ensureSchemaForDB(defaultNew, a.config.Timezone)
		for _, p := range existingLegacy {
			if filepath.Clean(p) == filepath.Clean(bestLegacy) {
				continue
			}
			_ = importLegacyTables(defaultNew, p)
		}

		a.logger.Info("✅ 旧数据迁移完成", "old", bestLegacy, "new", defaultNew)
		a.emitNotification("success", "旧数据已迁移", "已将旧版本数据库迁移到新数据库文件（避免新旧版本冲突）。")
		return
	}

	// 场景2：新库已存在，但用户反馈“历史数据丢失”（多见于新库先被创建为空库，导致复制迁移跳过）。
	// 这里按“缺什么补什么”的思路合并导入：INSERT OR IGNORE，不覆盖现有配置。
	_ = ensureSchemaForDB(defaultNew, a.config.Timezone)

	newReqCount, _ := countTableRows(defaultNew, "request_logs")
	newChCount, _ := countTableRows(defaultNew, "channels")
	newEpCount, _ := countTableRows(defaultNew, "endpoints")

	needImport := newReqCount == 0 || newChCount == 0 || newEpCount == 0
	if !needImport {
		return
	}

	importedAny := false
	for _, p := range existingLegacy {
		// 仅在“新库缺数据”时尝试导入，避免每次启动重复扫描大库。
		oldReqCount, _ := countTableRows(p, "request_logs")
		oldChCount, _ := countTableRows(p, "channels")
		oldEpCount, _ := countTableRows(p, "endpoints")
		if (newReqCount == 0 && oldReqCount > 0) || (newChCount == 0 && oldChCount > 0) || (newEpCount == 0 && oldEpCount > 0) {
			if err := importLegacyTables(defaultNew, p); err != nil {
				a.logger.Warn("⚠️ 旧数据导入失败", "old", p, "new", defaultNew, "error", err)
				continue
			}
			importedAny = true
		}
	}
	if importedAny {
		a.logger.Info("✅ 已导入旧数据", "new", defaultNew)
		a.emitNotification("success", "旧数据已导入", "已从旧数据库合并导入历史数据到当前数据库（不会覆盖现有配置）。")
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func countTableRows(dbPath, table string) (int64, error) {
	if dbPath == "" || table == "" {
		return 0, nil
	}
	dsn := dbPath + "?_busy_timeout=2000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// table 名仅允许字母数字下划线，避免拼接 SQL 注入
	for _, r := range table {
		if !(r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return 0, fmt.Errorf("invalid table name: %s", table)
		}
	}

	var count int64
	row := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table)
	if err := row.Scan(&count); err != nil {
		// 例如表不存在：当作 0
		return 0, nil
	}
	return count, nil
}

func ensureSchemaForDB(dbPath, timezone string) error {
	if dbPath == "" {
		return nil
	}
	adapter, err := tracking.NewDatabaseAdapter(tracking.DatabaseConfig{
		Type:         "sqlite",
		DatabasePath: dbPath,
		Timezone:     timezone,
	})
	if err != nil {
		return err
	}
	if err := adapter.Open(); err != nil {
		return err
	}
	defer adapter.Close()
	return adapter.InitSchema()
}

func importLegacyTables(dstDBPath, legacyDBPath string) error {
	if dstDBPath == "" || legacyDBPath == "" {
		return nil
	}

	dsn := dstDBPath + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=1&_busy_timeout=10000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	legacyEscaped := strings.ReplaceAll(legacyDBPath, "'", "''")
	if _, err := db.ExecContext(ctx, "ATTACH DATABASE '"+legacyEscaped+"' AS legacy"); err != nil {
		return fmt.Errorf("attach legacy database failed: %w", err)
	}
	defer func() {
		_, _ = db.ExecContext(context.Background(), "DETACH DATABASE legacy")
	}()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 导入历史数据（INSERT OR IGNORE），不覆盖新库中已存在的记录
	for _, table := range []string{"request_logs", "usage_summary", "endpoints", "channels", "model_pricing", "settings"} {
		cols, err := commonTableColumns(ctx, tx, table)
		if err != nil {
			continue
		}
		cols = filterOut(cols, "id")
		if len(cols) == 0 {
			continue
		}

		colList := make([]string, 0, len(cols))
		for _, c := range cols {
			colList = append(colList, `"`+c+`"`)
		}
		colCSV := strings.Join(colList, ", ")

		sqlStmt := fmt.Sprintf(
			"INSERT OR IGNORE INTO %s (%s) SELECT %s FROM legacy.%s",
			table, colCSV, colCSV, table,
		)
		if _, err := tx.ExecContext(ctx, sqlStmt); err != nil {
			continue
		}
	}

	return tx.Commit()
}

func commonTableColumns(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	mainCols, err := tableColumns(ctx, q, "main", table)
	if err != nil {
		return nil, err
	}
	legacyCols, err := tableColumns(ctx, q, "legacy", table)
	if err != nil {
		return nil, err
	}

	legacySet := make(map[string]struct{}, len(legacyCols))
	for _, c := range legacyCols {
		legacySet[c] = struct{}{}
	}
	common := make([]string, 0, len(mainCols))
	for _, c := range mainCols {
		if _, ok := legacySet[c]; ok {
			common = append(common, c)
		}
	}
	return common, nil
}

func tableColumns(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA "+schema+".table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name != "" {
			cols = append(cols, name)
		}
	}
	return cols, nil
}

func filterOut(cols []string, disallow string) []string {
	if disallow == "" {
		return cols
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c == disallow {
			continue
		}
		out = append(out, c)
	}
	return out
}

// setupProxyHandler 设置代理处理器
func (a *App) setupProxyHandler() {
	// 创建代理处理器
	a.proxyHandler = proxy.NewHandler(a.endpointManager, a.config)
	a.proxyHandler.SetEventBus(a.eventBus)

	// 创建中间件
	a.loggingMiddleware = middleware.NewLoggingMiddleware(a.logger)
	a.monitoringMiddleware = middleware.NewMonitoringMiddleware(a.endpointManager)
	a.authMiddleware = middleware.NewAuthMiddleware(a.config.Auth)

	// 连接组件
	a.monitoringMiddleware.SetEventBus(a.eventBus)
	a.loggingMiddleware.SetUsageTracker(a.usageTracker)
	a.loggingMiddleware.SetMonitoringMiddleware(a.monitoringMiddleware)
	a.proxyHandler.SetMonitoringMiddleware(a.monitoringMiddleware)

	if a.usageTracker != nil {
		a.proxyHandler.SetUsageTracker(a.usageTracker)
		if retryHandler := a.proxyHandler.GetRetryHandler(); retryHandler != nil {
			retryHandler.SetUsageTracker(a.usageTracker)
		}
	}
}

// startProxyServer 启动 HTTP 代理服务器
func (a *App) startProxyServer() {
	mux := http.NewServeMux()

	// 注册监控端点
	a.monitoringMiddleware.RegisterHealthEndpoint(mux)

	// 注册使用追踪健康检查端点
	if a.usageTracker != nil {
		mux.HandleFunc("/health/usage-tracker", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()

			if err := a.usageTracker.HealthCheck(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(fmt.Sprintf("Usage Tracker unhealthy: %v", err)))
				return
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Usage Tracker healthy"))
		})
	}

	// 注册代理处理器
	mux.Handle("/", a.loggingMiddleware.Wrap(a.authMiddleware.Wrap(a.proxyHandler)))

	// v5.1+ 端口探测：自动寻找可用端口
	var actualPort int
	var listener net.Listener
	var err error

	if a.portManager != nil {
		// 使用 PortManager 进行端口探测
		listener, actualPort, err = utils.FindAndBind(a.portManager.GetPreferredPort(), 10)
		if err != nil {
			a.logger.Error("❌ 无法找到可用端口", "error", err)
			a.emitError("代理服务器启动失败", "无法找到可用端口: "+err.Error())
			return
		}
		a.portManager.SetActualPort(actualPort)
	} else {
		// 回退到传统方式（首选端口）
		actualPort = a.config.Server.Port
		addr := fmt.Sprintf("%s:%d", a.config.Server.Host, actualPort)
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			a.logger.Error("❌ 端口绑定失败", "port", actualPort, "error", err)
			a.emitError("代理服务器启动失败", fmt.Sprintf("端口 %d 被占用: %v", actualPort, err))
			return
		}
	}

	// 更新配置中的实际端口
	a.config.Server.Port = actualPort

	a.proxyServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // 流式请求禁用写入超时
		IdleTimeout:  120 * time.Second,
	}

	// 在 goroutine 中启动服务器
	go func() {
		a.logger.Info("🌐 HTTP 代理服务器启动中...",
			"address", listener.Addr().String())

		if err := a.proxyServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			a.logger.Error("代理服务器启动失败", "error", err)
			// 通知前端
			a.emitError("代理服务器启动失败", err.Error())
		}
	}()

	// 等待服务器启动
	time.Sleep(100 * time.Millisecond)

	baseURL := fmt.Sprintf("http://%s:%d", a.config.Server.Host, actualPort)
	a.logger.Info("✅ 代理服务器启动成功",
		"url", baseURL)

	// 端口冲突提示
	if a.portManager != nil {
		portInfo := a.portManager.GetPortInfo()
		if portInfo.WasOccupied {
			a.logger.Warn(fmt.Sprintf("⚠️ 首选端口 %d 被占用，已自动切换到端口 %d",
				portInfo.PreferredPort, portInfo.ActualPort))
		}
	}

	// 安全警告
	if a.config.Server.Host != "127.0.0.1" && a.config.Server.Host != "localhost" && a.config.Server.Host != "::1" {
		if !a.config.Auth.Enabled {
			a.logger.Warn("⚠️  安全警告：服务器绑定到非本地地址但未启用鉴权！")
		}
	}
}

// setupConfigReload 设置配置热重载
func (a *App) setupConfigReload() {
	a.configWatcher.AddReloadCallback(func(newCfg *config.Config) {
		a.mu.Lock()
		defer a.mu.Unlock()

		// 更新配置引用
		a.config = newCfg

		// 停止旧的日志 Emitter，避免多个 Emitter 同时广播导致日志重复
		if a.logEmitter != nil {
			a.logEmitter.Stop()
		}

		// 更新日志
		newLogger, newBroadcastHandler := setupLogger(newCfg.Logging)
		slog.SetDefault(newLogger)
		a.logger = newLogger
		a.logHandler = newBroadcastHandler
		a.logEmitter = newBroadcastHandler.Emitter

		// 更新各组件
		a.configWatcher.UpdateLogger(newLogger)
		a.endpointManager.UpdateConfig(newCfg)
		a.proxyHandler.UpdateConfig(newCfg)
		a.authMiddleware.UpdateConfig(newCfg.Auth)

		// v5.0+ 注意：模型定价不再从 config.yaml 热重载
		// 定价配置通过前端「定价」页面管理，存储在 SQLite model_pricing 表中

		a.logger.Info("🔄 配置已重新加载")

		// 通知前端配置已更新
		a.emitConfigReloaded()
	})

	a.logger.Info("🔄 配置热重载已启用")
}

// setupEventBridges 设置事件桥接
// 将内部 EventBus 事件转发到 Wails 前端
func (a *App) setupEventBridges() {
	// 注意：当前 EventBus 实现不支持订阅回调
	// 我们使用定时轮询来更新前端状态
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				if a.isRunning {
					a.emitSystemStatus()
				}
			}
		}
	}()

	a.logger.Info("📡 事件桥接已启用")
}

// startHistoryCollector 启动历史数据收集器
// 定期收集 Metrics 历史数据点，用于图表显示
func (a *App) startHistoryCollector() {
	if a.monitoringMiddleware == nil {
		a.logger.Warn("⚠️  监控中间件未初始化，跳过历史数据收集器启动")
		return
	}

	// 立即收集一次初始数据点
	// 注意：必须直接在原始 *Metrics 上调用 AddHistoryDataPoints()
	// 不能调用 GetMetrics() 获取副本，因为那样修改的是副本而不是原始数据
	metrics := a.monitoringMiddleware.GetMetrics()
	if metrics != nil {
		metrics.AddHistoryDataPoints()
		a.logger.Info("📊 初始历史数据点已收集")
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second) // 每30秒收集一次
		defer ticker.Stop()

		a.logger.Info("📊 历史数据收集器已启动 (30秒间隔)")

		for {
			select {
			case <-a.ctx.Done():
				a.logger.Info("📊 历史数据收集器已停止")
				return
			case <-ticker.C:
				// 收集历史数据点
				// 直接在原始 *Metrics 上调用，而不是获取副本
				metrics := a.monitoringMiddleware.GetMetrics()
				if metrics != nil {
					metrics.AddHistoryDataPoints()
				}
			}
		}
	}()
}

// emitError 发送错误通知到前端
func (a *App) emitError(title, message string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "error", map[string]string{
			"title":   title,
			"message": message,
		})
	}
}

// emitConfigReloaded 通知前端配置已重载
func (a *App) emitConfigReloaded() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "config:reloaded", nil)
	}
}

// setupSettingsStore 设置系统设置存储 (v5.1+ SQLite)
func (a *App) setupSettingsStore() {
	// 获取数据库连接：优先使用 storeDB
	db := a.storeDB
	if db == nil && a.usageTracker != nil {
		db = a.usageTracker.GetDB()
	}
	if db == nil {
		a.logger.Error("❌ 无法获取数据库连接 (设置存储)")
		return
	}

	// 创建 SettingsStore
	a.settingsStore = store.NewSQLiteSettingsStore(db)

	// 创建 SettingsService
	a.settingsService = service.NewSettingsService(a.settingsStore)

	// 设置配置变更回调 - 热更新
	a.settingsService.SetOnChangeCallback(func() {
		a.applySettingsToConfig()
	})

	// 初始化默认设置（如果表为空）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.settingsService.InitDefaults(ctx); err != nil {
		a.logger.Error("❌ 初始化默认设置失败", "error", err)
		return
	}

	// 从数据库加载设置并应用到配置
	a.applySettingsToConfig()

	// 初始化端口管理器
	preferredPort := a.settingsService.GetInt(ctx, service.CategoryServer, "preferred_port", a.config.Server.Port)
	a.portManager = utils.NewPortManager(preferredPort)

	a.logger.Info("✅ 系统设置存储已启用 (SQLite)")
}

// applySettingsToConfig 从数据库加载设置并应用到运行时配置
func (a *App) applySettingsToConfig() {
	if a.settingsService == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 策略配置
	a.config.Strategy.Type = a.getSettingString(ctx, service.CategoryStrategy, "type", a.config.Strategy.Type)
	a.config.Strategy.FastTestEnabled = a.settingsService.GetBool(ctx, service.CategoryStrategy, "fast_test_enabled", a.config.Strategy.FastTestEnabled)
	a.config.Strategy.FastTestCacheTTL = a.settingsService.GetDuration(ctx, service.CategoryStrategy, "fast_test_cache_ttl", a.config.Strategy.FastTestCacheTTL)
	a.config.Strategy.FastTestTimeout = a.settingsService.GetDuration(ctx, service.CategoryStrategy, "fast_test_timeout", a.config.Strategy.FastTestTimeout)
	a.config.Strategy.FastTestPath = a.getSettingString(ctx, service.CategoryStrategy, "fast_test_path", a.config.Strategy.FastTestPath)

	// 重试配置
	a.config.Retry.MaxAttempts = a.settingsService.GetInt(ctx, service.CategoryRetry, "max_attempts", a.config.Retry.MaxAttempts)
	a.config.Retry.BaseDelay = a.settingsService.GetDuration(ctx, service.CategoryRetry, "base_delay", a.config.Retry.BaseDelay)
	a.config.Retry.MaxDelay = a.settingsService.GetDuration(ctx, service.CategoryRetry, "max_delay", a.config.Retry.MaxDelay)
	a.config.Retry.Multiplier = a.settingsService.GetFloat(ctx, service.CategoryRetry, "multiplier", a.config.Retry.Multiplier)

	// 健康检查配置
	a.config.Health.CheckInterval = a.settingsService.GetDuration(ctx, service.CategoryHealth, "check_interval", a.config.Health.CheckInterval)
	a.config.Health.Timeout = a.settingsService.GetDuration(ctx, service.CategoryHealth, "timeout", a.config.Health.Timeout)
	a.config.Health.HealthPath = a.getSettingString(ctx, service.CategoryHealth, "health_path", a.config.Health.HealthPath)

	// 故障转移配置
	a.config.Failover.Enabled = a.settingsService.GetBool(ctx, service.CategoryFailover, "enabled", a.config.Failover.Enabled)
	a.config.Failover.DefaultCooldown = a.settingsService.GetDuration(ctx, service.CategoryFailover, "default_cooldown", a.config.Failover.DefaultCooldown)

	// 请求控制配置
	a.config.GlobalTimeout = a.settingsService.GetDuration(ctx, service.CategoryRequest, "global_timeout", a.config.GlobalTimeout)
	a.config.RequestSuspend.Enabled = a.settingsService.GetBool(ctx, service.CategoryRequest, "suspend_enabled", a.config.RequestSuspend.Enabled)
	a.config.RequestSuspend.Timeout = a.settingsService.GetDuration(ctx, service.CategoryRequest, "suspend_timeout", a.config.RequestSuspend.Timeout)
	a.config.RequestSuspend.MaxSuspendedRequests = a.settingsService.GetInt(ctx, service.CategoryRequest, "max_suspended", a.config.RequestSuspend.MaxSuspendedRequests)
	a.config.RequestSuspend.EOFRetryHint = a.settingsService.GetBool(ctx, service.CategoryRequest, "eof_retry_hint", a.config.RequestSuspend.EOFRetryHint)

	// 流式传输配置
	a.config.Streaming.HeartbeatInterval = a.settingsService.GetDuration(ctx, service.CategoryStreaming, "heartbeat_interval", a.config.Streaming.HeartbeatInterval)
	a.config.Streaming.ReadTimeout = a.settingsService.GetDuration(ctx, service.CategoryStreaming, "read_timeout", a.config.Streaming.ReadTimeout)
	a.config.Streaming.MaxIdleTime = a.settingsService.GetDuration(ctx, service.CategoryStreaming, "max_idle_time", a.config.Streaming.MaxIdleTime)
	a.config.Streaming.ResponseHeaderTimeout = a.settingsService.GetDuration(ctx, service.CategoryStreaming, "response_header_timeout", a.config.Streaming.ResponseHeaderTimeout)

	// 访问控制配置
	a.config.Auth.Enabled = a.settingsService.GetBool(ctx, service.CategoryAuth, "enabled", a.config.Auth.Enabled)
	a.config.Auth.Token = a.getSettingString(ctx, service.CategoryAuth, "token", a.config.Auth.Token)

	// Token 计数配置
	a.config.TokenCounting.Enabled = a.settingsService.GetBool(ctx, service.CategoryTokenCounting, "enabled", a.config.TokenCounting.Enabled)
	a.config.TokenCounting.EstimationRatio = a.settingsService.GetFloat(ctx, service.CategoryTokenCounting, "estimation_ratio", a.config.TokenCounting.EstimationRatio)

	// 数据保留配置
	a.config.UsageTracking.RetentionDays = a.settingsService.GetInt(ctx, service.CategoryRetention, "retention_days", a.config.UsageTracking.RetentionDays)
	a.config.UsageTracking.CleanupInterval = a.settingsService.GetDuration(ctx, service.CategoryRetention, "cleanup_interval", a.config.UsageTracking.CleanupInterval)

	a.logger.Debug("已从数据库加载设置")

	// 更新各组件配置
	if a.endpointManager != nil {
		a.endpointManager.UpdateConfig(a.config)
	}
	if a.proxyHandler != nil {
		a.proxyHandler.UpdateConfig(a.config)
	}
	if a.authMiddleware != nil {
		a.authMiddleware.UpdateConfig(a.config.Auth)
	}
}

// getSettingString 获取字符串设置值（带默认值）
func (a *App) getSettingString(ctx context.Context, category, key, defaultVal string) string {
	val, err := a.settingsService.GetValue(ctx, category, key)
	if err != nil || val == "" {
		return defaultVal
	}
	return val
}
