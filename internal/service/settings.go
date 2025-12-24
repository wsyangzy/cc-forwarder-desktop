// Package service 提供业务逻辑层实现
// 设置服务 - v5.1.0 新增 (2025-12-08)
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"cc-forwarder/internal/store"
)

// SettingCategory 设置分类常量
const (
	CategoryStrategy      = "strategy"
	CategoryRetry         = "retry"
	CategoryHealth        = "health"
	CategoryFailover      = "failover"
	CategoryRequest       = "request"
	CategoryStreaming     = "streaming"
	CategoryAuth          = "auth"
	CategoryTokenCounting = "token_counting"
	CategoryRetention     = "retention"
	CategoryHotPool       = "hot_pool"
	CategoryServer        = "server"
)

// SettingValueType 设置值类型常量
const (
	ValueTypeString   = "string"
	ValueTypeInt      = "int"
	ValueTypeFloat    = "float"
	ValueTypeBool     = "bool"
	ValueTypeDuration = "duration"
	ValueTypePassword = "password"
	ValueTypeJSON     = "json"
)

// CategoryInfo 分类信息
type CategoryInfo struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Order       int    `json:"order"`
}

// SettingsService 设置管理业务服务
type SettingsService struct {
	store          store.SettingsStore
	onChangeFunc   func() // 配置变更回调
	categoryLabels map[string]CategoryInfo
}

// NewSettingsService 创建设置服务实例
func NewSettingsService(store store.SettingsStore) *SettingsService {
	svc := &SettingsService{
		store: store,
		categoryLabels: map[string]CategoryInfo{
			CategoryAuth: {
				Name:        CategoryAuth,
				Label:       "访问鉴权",
				Description: "配置代理服务的访问令牌",
				Icon:        "🔐",
				Order:       1,
			},
			CategoryStrategy: {
				Name:        CategoryStrategy,
				Label:       "路由策略",
				Description: "配置请求路由策略和快速测试",
				Icon:        "🔀",
				Order:       2,
			},
			CategoryRetry: {
				Name:        CategoryRetry,
				Label:       "重试配置",
				Description: "配置请求失败重试行为",
				Icon:        "🔄",
				Order:       3,
			},
			CategoryHealth: {
				Name:        CategoryHealth,
				Label:       "健康检查",
				Description: "配置端点健康检查参数",
				Icon:        "❤️",
				Order:       4,
			},
			CategoryFailover: {
				Name:        CategoryFailover,
				Label:       "故障转移",
				Description: "配置渠道间故障转移行为",
				Icon:        "🔁",
				Order:       5,
			},
			CategoryRequest: {
				Name:        CategoryRequest,
				Label:       "请求控制",
				Description: "配置全局超时和请求挂起",
				Icon:        "⏱️",
				Order:       6,
			},
			// CategoryStreaming 和 CategoryHotPool 不在前端设置页面展示
			// 这些是底层技术配置，保留在 config.yaml 中
			CategoryTokenCounting: {
				Name:        CategoryTokenCounting,
				Label:       "Token 计数",
				Description: "配置 Token 计数功能",
				Icon:        "🔢",
				Order:       7,
			},
			CategoryRetention: {
				Name:        CategoryRetention,
				Label:       "数据保留",
				Description: "配置历史数据保留策略",
				Icon:        "📦",
				Order:       8,
			},
			CategoryServer: {
				Name:        CategoryServer,
				Label:       "服务端口",
				Description: "配置 API 服务端口",
				Icon:        "📡",
				Order:       0, // 最前（特殊处理，单独显示）
			},
		},
	}
	return svc
}

// SetOnChangeCallback 设置配置变更回调
func (s *SettingsService) SetOnChangeCallback(fn func()) {
	s.onChangeFunc = fn
}

// GetCategories 获取所有分类信息
func (s *SettingsService) GetCategories() []CategoryInfo {
	categories := make([]CategoryInfo, 0, len(s.categoryLabels))
	for _, info := range s.categoryLabels {
		categories = append(categories, info)
	}
	// 按 Order 排序
	for i := 0; i < len(categories)-1; i++ {
		for j := i + 1; j < len(categories); j++ {
			if categories[i].Order > categories[j].Order {
				categories[i], categories[j] = categories[j], categories[i]
			}
		}
	}
	return categories
}

// GetCategoryInfo 获取分类信息
func (s *SettingsService) GetCategoryInfo(category string) *CategoryInfo {
	if info, ok := s.categoryLabels[category]; ok {
		return &info
	}
	return nil
}

// Get 获取单个设置值
func (s *SettingsService) Get(ctx context.Context, category, key string) (*store.SettingRecord, error) {
	return s.store.Get(ctx, category, key)
}

// GetValue 获取设置值（仅返回值字符串）
func (s *SettingsService) GetValue(ctx context.Context, category, key string) (string, error) {
	record, err := s.store.Get(ctx, category, key)
	if err != nil {
		return "", err
	}
	if record == nil {
		return "", nil
	}
	return record.Value, nil
}

// GetInt 获取整数值
func (s *SettingsService) GetInt(ctx context.Context, category, key string, defaultVal int) int {
	val, err := s.GetValue(ctx, category, key)
	if err != nil || val == "" {
		return defaultVal
	}
	if i, err := strconv.Atoi(val); err == nil {
		return i
	}
	return defaultVal
}

// GetFloat 获取浮点数值
func (s *SettingsService) GetFloat(ctx context.Context, category, key string, defaultVal float64) float64 {
	val, err := s.GetValue(ctx, category, key)
	if err != nil || val == "" {
		return defaultVal
	}
	if f, err := strconv.ParseFloat(val, 64); err == nil {
		return f
	}
	return defaultVal
}

// GetBool 获取布尔值
func (s *SettingsService) GetBool(ctx context.Context, category, key string, defaultVal bool) bool {
	val, err := s.GetValue(ctx, category, key)
	if err != nil || val == "" {
		return defaultVal
	}
	return val == "true" || val == "1" || val == "yes"
}

// GetDuration 获取时间间隔值
func (s *SettingsService) GetDuration(ctx context.Context, category, key string, defaultVal time.Duration) time.Duration {
	val, err := s.GetValue(ctx, category, key)
	if err != nil || val == "" {
		return defaultVal
	}
	if d, err := time.ParseDuration(val); err == nil {
		return d
	}
	return defaultVal
}

// GetByCategory 获取分类下的所有设置
func (s *SettingsService) GetByCategory(ctx context.Context, category string) ([]*store.SettingRecord, error) {
	return s.store.GetByCategory(ctx, category)
}

// GetAll 获取所有设置
func (s *SettingsService) GetAll(ctx context.Context) ([]*store.SettingRecord, error) {
	return s.store.GetAll(ctx)
}

// Set 设置单个值
func (s *SettingsService) Set(ctx context.Context, category, key, value string) error {
	if err := s.store.Set(ctx, category, key, value); err != nil {
		return err
	}
	s.triggerOnChange(category, key)
	return nil
}

// BatchSet 批量设置（不触发回调，需手动触发）
func (s *SettingsService) BatchSet(ctx context.Context, records []*store.SettingRecord) error {
	return s.store.BatchSet(ctx, records)
}

// UpdateAndApply 批量更新并应用（触发热更新）
// 只更新 value，保留 label、description 等元数据
func (s *SettingsService) UpdateAndApply(ctx context.Context, records []*store.SettingRecord) error {
	if err := s.store.BatchUpdateValues(ctx, records); err != nil {
		return fmt.Errorf("保存设置失败: %w", err)
	}

	// 触发配置热更新
	if s.onChangeFunc != nil {
		s.onChangeFunc()
		slog.Info("✅ [SettingsService] 设置已保存并应用热更新")
	}

	return nil
}

// ResetCategory 重置分类设置为默认值
func (s *SettingsService) ResetCategory(ctx context.Context, category string) error {
	// 删除当前分类的所有设置
	if err := s.store.DeleteByCategory(ctx, category); err != nil {
		return fmt.Errorf("删除分类设置失败: %w", err)
	}

	// 重新初始化默认值
	defaults := s.getDefaultsForCategory(category)
	if len(defaults) > 0 {
		if err := s.store.BatchSet(ctx, defaults); err != nil {
			return fmt.Errorf("重置默认值失败: %w", err)
		}
	}

	// 触发热更新
	if s.onChangeFunc != nil {
		s.onChangeFunc()
	}

	slog.Info(fmt.Sprintf("✅ [SettingsService] 分类 %s 已重置为默认值", category))
	return nil
}

// triggerOnChange 触发变更回调（检查是否需要重启）
func (s *SettingsService) triggerOnChange(category, key string) {
	record, _ := s.store.Get(context.Background(), category, key)
	if record != nil && record.RequiresRestart {
		slog.Info(fmt.Sprintf("⚠️ [SettingsService] 设置 %s.%s 已保存，需要重启生效", category, key))
		return // 需要重启的配置不触发热更新
	}

	if s.onChangeFunc != nil {
		s.onChangeFunc()
	}
}

// InitDefaults 初始化默认设置
func (s *SettingsService) InitDefaults(ctx context.Context) error {
	defaults := s.GetAllDefaults()

	// 始终同步元数据（label、description、value_type等）
	// 这样即使数据库中已有数据，也能更新到最新的元数据
	// 但会保留用户设置的 value 值
	return s.store.SyncMetadata(ctx, defaults)
}

// IsInitialized 检查是否已初始化
func (s *SettingsService) IsInitialized(ctx context.Context) (bool, error) {
	return s.store.IsInitialized(ctx)
}

// GetAllDefaults 获取所有默认设置
func (s *SettingsService) GetAllDefaults() []*store.SettingRecord {
	var defaults []*store.SettingRecord

	// Server 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryServer)...)

	// Strategy 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryStrategy)...)

	// Retry 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryRetry)...)

	// Health 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryHealth)...)

	// Failover 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryFailover)...)

	// Request 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryRequest)...)

	// Streaming 和 HotPool 不写入数据库，保留在 config.yaml 中

	// Auth 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryAuth)...)

	// TokenCounting 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryTokenCounting)...)

	// Retention 设置
	defaults = append(defaults, s.getDefaultsForCategory(CategoryRetention)...)

	return defaults
}

// getDefaultsForCategory 获取分类的默认设置
func (s *SettingsService) getDefaultsForCategory(category string) []*store.SettingRecord {
	switch category {
	case CategoryServer:
		return []*store.SettingRecord{
			{Category: CategoryServer, Key: "preferred_port", Value: "8087", ValueType: ValueTypeInt, Label: "首选端口", Description: "API 服务首选端口，被占用时自动递增", DisplayOrder: 1, RequiresRestart: true},
		}

	case CategoryStrategy:
		return []*store.SettingRecord{
			{Category: CategoryStrategy, Key: "type", Value: "priority", ValueType: ValueTypeString, Label: "策略类型", Description: "路由策略：priority（优先级）或 fastest（最快响应）。渠道内用于端点选择；启用渠道间故障转移时，渠道间也按该策略选择目标渠道。", DisplayOrder: 1},
			{Category: CategoryStrategy, Key: "fast_test_enabled", Value: "true", ValueType: ValueTypeBool, Label: "启用快速测试", Description: "仅在 fastest 策略下生效", DisplayOrder: 2},
			{Category: CategoryStrategy, Key: "fast_test_cache_ttl", Value: "3s", ValueType: ValueTypeDuration, Label: "缓存时间", Description: "快速测试结果缓存时间", DisplayOrder: 3},
			{Category: CategoryStrategy, Key: "fast_test_timeout", Value: "1s", ValueType: ValueTypeDuration, Label: "测试超时", Description: "快速测试超时时间", DisplayOrder: 4},
			{Category: CategoryStrategy, Key: "fast_test_path", Value: "/v1/models", ValueType: ValueTypeString, Label: "测试路径", Description: "快速测试请求路径", DisplayOrder: 5},
		}

	case CategoryRetry:
		return []*store.SettingRecord{
			{Category: CategoryRetry, Key: "max_attempts", Value: "3", ValueType: ValueTypeInt, Label: "最大重试次数", Description: "请求失败后的最大重试次数", DisplayOrder: 1},
			{Category: CategoryRetry, Key: "base_delay", Value: "1s", ValueType: ValueTypeDuration, Label: "基础延迟", Description: "首次重试前的等待时间", DisplayOrder: 2},
			{Category: CategoryRetry, Key: "max_delay", Value: "30s", ValueType: ValueTypeDuration, Label: "最大延迟", Description: "重试延迟的上限", DisplayOrder: 3},
			{Category: CategoryRetry, Key: "multiplier", Value: "2.0", ValueType: ValueTypeFloat, Label: "延迟倍数", Description: "每次重试延迟的倍增系数", DisplayOrder: 4},
		}

	case CategoryHealth:
		return []*store.SettingRecord{
			{Category: CategoryHealth, Key: "check_interval", Value: "30s", ValueType: ValueTypeDuration, Label: "检查间隔", Description: "健康检查的时间间隔", DisplayOrder: 1},
			{Category: CategoryHealth, Key: "timeout", Value: "5s", ValueType: ValueTypeDuration, Label: "检查超时", Description: "健康检查的超时时间", DisplayOrder: 2},
			{Category: CategoryHealth, Key: "health_path", Value: "/v1/models", ValueType: ValueTypeString, Label: "检查路径", Description: "健康检查请求的 API 路径", DisplayOrder: 3},
		}

	case CategoryFailover:
		return []*store.SettingRecord{
			{Category: CategoryFailover, Key: "enabled", Value: "true", ValueType: ValueTypeBool, Label: "渠道间故障转移", Description: "当前渠道内所有端点均重试耗尽时，自动切换到备用渠道。渠道内端点切换默认开启，可通过端点「参与渠道内故障转移」关闭。备用渠道的选择遵循「路由策略」并跳过暂停/冷却渠道。", DisplayOrder: 1},
			{Category: CategoryFailover, Key: "default_cooldown", Value: "600s", ValueType: ValueTypeDuration, Label: "默认冷却时间", Description: "请求级故障转移触发后，失败端点/失败渠道进入冷却的等待时间", DisplayOrder: 2},
		}

	case CategoryRequest:
		return []*store.SettingRecord{
			{Category: CategoryRequest, Key: "global_timeout", Value: "300s", ValueType: ValueTypeDuration, Label: "全局超时", Description: "非流式请求的默认超时时间", DisplayOrder: 1},
			{Category: CategoryRequest, Key: "suspend_enabled", Value: "false", ValueType: ValueTypeBool, Label: "启用请求挂起", Description: "当所有端点不可用时挂起请求等待恢复", DisplayOrder: 2},
			{Category: CategoryRequest, Key: "suspend_timeout", Value: "300s", ValueType: ValueTypeDuration, Label: "挂起超时", Description: "请求挂起的最大等待时间", DisplayOrder: 3},
			{Category: CategoryRequest, Key: "max_suspended", Value: "100", ValueType: ValueTypeInt, Label: "最大挂起数", Description: "同时挂起的最大请求数量", DisplayOrder: 4},
			{Category: CategoryRequest, Key: "eof_retry_hint", Value: "false", ValueType: ValueTypeBool, Label: "EOF 重试提示", Description: "流式传输中断时发送可重试错误格式，让客户端自动重试", DisplayOrder: 5},
		}

	case CategoryStreaming:
		return []*store.SettingRecord{
			{Category: CategoryStreaming, Key: "heartbeat_interval", Value: "30s", ValueType: ValueTypeDuration, Label: "心跳间隔", Description: "流式连接心跳间隔", DisplayOrder: 1},
			{Category: CategoryStreaming, Key: "read_timeout", Value: "10s", ValueType: ValueTypeDuration, Label: "读取超时", Description: "流式数据读取超时", DisplayOrder: 2},
			{Category: CategoryStreaming, Key: "max_idle_time", Value: "120s", ValueType: ValueTypeDuration, Label: "最大空闲时间", Description: "流式连接最大空闲时间", DisplayOrder: 3},
			{Category: CategoryStreaming, Key: "response_header_timeout", Value: "60s", ValueType: ValueTypeDuration, Label: "响应头超时", Description: "等待服务端首次响应头的超时时间", DisplayOrder: 4},
		}

	case CategoryAuth:
		return []*store.SettingRecord{
			{Category: CategoryAuth, Key: "enabled", Value: "false", ValueType: ValueTypeBool, Label: "启用鉴权", Description: "是否启用 API 访问鉴权", DisplayOrder: 1},
			{Category: CategoryAuth, Key: "token", Value: "", ValueType: ValueTypePassword, Label: "鉴权 Token", Description: "Bearer Token 值", DisplayOrder: 2},
		}

	case CategoryTokenCounting:
		return []*store.SettingRecord{
			{Category: CategoryTokenCounting, Key: "enabled", Value: "true", ValueType: ValueTypeBool, Label: "启用 Token 计数", Description: "是否启用 count_tokens 端点支持", DisplayOrder: 1},
			{Category: CategoryTokenCounting, Key: "estimation_ratio", Value: "4.0", ValueType: ValueTypeFloat, Label: "估算比例", Description: "Token 估算比例 (1 token ≈ N 字符)", DisplayOrder: 2},
		}

	case CategoryRetention:
		return []*store.SettingRecord{
			{Category: CategoryRetention, Key: "retention_days", Value: "0", ValueType: ValueTypeInt, Label: "数据保留天数", Description: "请求日志保留天数，0 表示永久保留", DisplayOrder: 1},
			{Category: CategoryRetention, Key: "cleanup_interval", Value: "24h", ValueType: ValueTypeDuration, Label: "清理间隔", Description: "自动清理任务的执行间隔", DisplayOrder: 2},
		}

	case CategoryHotPool:
		return []*store.SettingRecord{
			{Category: CategoryHotPool, Key: "enabled", Value: "true", ValueType: ValueTypeBool, Label: "启用热池", Description: "是否启用内存热池模式", DisplayOrder: 1},
			{Category: CategoryHotPool, Key: "max_age", Value: "30m", ValueType: ValueTypeDuration, Label: "最大存活时间", Description: "请求在热池中的最大存活时间", DisplayOrder: 2},
			{Category: CategoryHotPool, Key: "max_size", Value: "10000", ValueType: ValueTypeInt, Label: "最大容量", Description: "热池最大请求数量", DisplayOrder: 3},
			{Category: CategoryHotPool, Key: "cleanup_interval", Value: "1m", ValueType: ValueTypeDuration, Label: "清理间隔", Description: "僵尸请求清理间隔", DisplayOrder: 4},
			{Category: CategoryHotPool, Key: "archive_on_cleanup", Value: "true", ValueType: ValueTypeBool, Label: "清理时归档", Description: "清理时是否将请求归档到数据库", DisplayOrder: 5},
		}

	default:
		return nil
	}
}
