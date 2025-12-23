// app_api_storage.go - v5.0+ 端点存储管理 API (Wails Bindings)
// 提供 SQLite 端点存储的增删改查功能

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"cc-forwarder/internal/endpoint"
	"cc-forwarder/internal/store"
)

// ============================================================
// v5.0+ 端点存储管理 API (SQLite)
// ============================================================
// 这些 API 仅在 endpoints_storage.type 为 "sqlite" 时可用
// 提供端点的增删改查功能

// EndpointRecordInfo 端点记录信息（给前端用的结构体）
type EndpointRecordInfo struct {
	ID                          int64             `json:"id"`
	Channel                     string            `json:"channel"`
	Name                        string            `json:"name"`
	URL                         string            `json:"url"`
	Token                       string            `json:"token"`        // v5.0: 本地桌面应用，直接返回原始 Token
	ApiKey                      string            `json:"api_key"`      // v5.0: 本地桌面应用，直接返回原始 ApiKey
	TokenMasked                 string            `json:"token_masked"` // 脱敏后的 Token（列表展示用）
	ApiKeyMasked                string            `json:"api_key_masked"`
	Headers                     map[string]string `json:"headers"`
	Priority                    int               `json:"priority"`
	FailoverEnabled             bool              `json:"failover_enabled"`
	CooldownSeconds             *int              `json:"cooldown_seconds"`
	TimeoutSeconds              int               `json:"timeout_seconds"`
	SupportsCountTokens         bool              `json:"supports_count_tokens"`
	CostMultiplier              float64           `json:"cost_multiplier"`
	InputCostMultiplier         float64           `json:"input_cost_multiplier"`
	OutputCostMultiplier        float64           `json:"output_cost_multiplier"`
	CacheCreationCostMultiplier float64           `json:"cache_creation_cost_multiplier"`
	CacheReadCostMultiplier     float64           `json:"cache_read_cost_multiplier"`
	Enabled                     bool              `json:"enabled"`
	CreatedAt                   string            `json:"created_at"`
	UpdatedAt                   string            `json:"updated_at"`
	// 运行时健康状态
	Healthy        bool    `json:"healthy"`
	LastCheck      string  `json:"last_check"` // 最后健康检查时间
	ResponseTimeMs float64 `json:"response_time_ms"`
	// 冷却状态（请求级故障转移）
	InCooldown     bool   `json:"in_cooldown"`     // 是否处于冷却中
	CooldownUntil  string `json:"cooldown_until"`  // 冷却截止时间
	CooldownReason string `json:"cooldown_reason"` // 冷却原因
}

// CreateEndpointInput 创建端点的输入参数
type CreateEndpointInput struct {
	Channel                       string            `json:"channel"`
	Name                          string            `json:"name"`
	URL                           string            `json:"url"`
	Token                         string            `json:"token"`
	ApiKey                        string            `json:"api_key"`
	Headers                       map[string]string `json:"headers"`
	Priority                      int               `json:"priority"`
	FailoverEnabled               bool              `json:"failover_enabled"`
	CooldownSeconds               *int              `json:"cooldown_seconds"`
	TimeoutSeconds                int               `json:"timeout_seconds"`
	SupportsCountTokens           bool              `json:"supports_count_tokens"`
	CostMultiplier                float64           `json:"cost_multiplier"`
	InputCostMultiplier           float64           `json:"input_cost_multiplier"`
	OutputCostMultiplier          float64           `json:"output_cost_multiplier"`
	CacheCreationCostMultiplier   float64           `json:"cache_creation_cost_multiplier"`
	CacheCreationCostMultiplier1h float64           `json:"cache_creation_cost_multiplier_1h"`
	CacheReadCostMultiplier       float64           `json:"cache_read_cost_multiplier"`
}

// EndpointStorageStatus 端点存储状态
type EndpointStorageStatus struct {
	Enabled      bool   `json:"enabled"`       // 是否启用 SQLite 存储
	StorageType  string `json:"storage_type"`  // "yaml" 或 "sqlite"
	TotalCount   int    `json:"total_count"`   // 端点总数
	EnabledCount int    `json:"enabled_count"` // 已启用端点数
}

// GetEndpointStorageStatus 获取端点存储状态
func (a *App) GetEndpointStorageStatus() EndpointStorageStatus {
	a.mu.RLock()
	cfg := a.config
	endpointService := a.endpointService
	a.mu.RUnlock()

	status := EndpointStorageStatus{
		StorageType: "yaml", // 默认
	}

	if cfg != nil {
		status.StorageType = cfg.EndpointsStorage.Type
	}

	// 如果使用 SQLite 存储且服务已初始化
	if endpointService != nil {
		status.Enabled = true

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		records, err := endpointService.ListEndpoints(ctx)
		if err == nil {
			status.TotalCount = len(records)
			for _, r := range records {
				if r.Enabled {
					status.EnabledCount++
				}
			}
		}
	}

	return status
}

// GetEndpointRecords 获取所有端点记录（SQLite 存储）
func (a *App) GetEndpointRecords() ([]EndpointRecordInfo, error) {
	a.mu.RLock()
	endpointService := a.endpointService
	endpointManager := a.endpointManager
	a.mu.RUnlock()

	if endpointService == nil {
		return nil, fmt.Errorf("端点存储服务未启用 (需要设置 endpoints_storage.type: sqlite)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	records, err := endpointService.ListEndpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取端点列表失败: %w", err)
	}

	result := make([]EndpointRecordInfo, 0, len(records))
	for _, r := range records {
		info := a.recordToInfo(r)

		// 获取运行时健康状态
		if endpointManager != nil {
			status := endpointManager.GetEndpointStatus(r.Name)
			info.Healthy = status.Healthy
			info.ResponseTimeMs = float64(status.ResponseTime.Milliseconds())
			// 格式化最后检查时间
			if !status.LastCheck.IsZero() {
				info.LastCheck = status.LastCheck.Format("2006-01-02 15:04:05")
			}
			// 冷却状态
			if !status.CooldownUntil.IsZero() && status.CooldownUntil.After(time.Now()) {
				info.InCooldown = true
				info.CooldownUntil = status.CooldownUntil.Format("2006-01-02 15:04:05")
				info.CooldownReason = status.CooldownReason
			}
		}

		result = append(result, info)
	}

	return result, nil
}

// GetEndpointRecord 获取单个端点详情
func (a *App) GetEndpointRecord(name string) (EndpointRecordInfo, error) {
	a.mu.RLock()
	endpointService := a.endpointService
	a.mu.RUnlock()

	if endpointService == nil {
		return EndpointRecordInfo{}, fmt.Errorf("端点存储服务未启用")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	detail, err := endpointService.GetEndpointWithHealth(ctx, name)
	if err != nil {
		return EndpointRecordInfo{}, err
	}

	// 转换为 EndpointRecordInfo
	info := EndpointRecordInfo{
		Name:    name,
		Enabled: true,
	}

	// 从 map 中提取字段
	if v, ok := detail["id"].(int64); ok {
		info.ID = v
	}
	if v, ok := detail["channel"].(string); ok {
		info.Channel = v
	}
	if v, ok := detail["url"].(string); ok {
		info.URL = v
	}
	if v, ok := detail["token_masked"].(string); ok {
		info.TokenMasked = v
	}
	if v, ok := detail["priority"].(int); ok {
		info.Priority = v
	}
	if v, ok := detail["failover_enabled"].(bool); ok {
		info.FailoverEnabled = v
	}
	if v, ok := detail["timeout_seconds"].(int); ok {
		info.TimeoutSeconds = v
	}
	if v, ok := detail["cost_multiplier"].(float64); ok {
		info.CostMultiplier = v
	}
	if v, ok := detail["enabled"].(bool); ok {
		info.Enabled = v
	}

	// 健康状态
	if health, ok := detail["health"].(map[string]interface{}); ok {
		if v, ok := health["healthy"].(bool); ok {
			info.Healthy = v
		}
		if v, ok := health["response_time_ms"].(int64); ok {
			info.ResponseTimeMs = float64(v)
		}
	}

	return info, nil
}

// CreateEndpointRecord 创建新端点
func (a *App) CreateEndpointRecord(input CreateEndpointInput) error {
	a.mu.RLock()
	endpointService := a.endpointService
	channelService := a.channelService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}

	// 设置默认值
	if input.Priority == 0 {
		input.Priority = 1
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 300
	}
	if input.CostMultiplier == 0 {
		input.CostMultiplier = 1.0
	}
	if input.CacheCreationCostMultiplier1h == 0 {
		input.CacheCreationCostMultiplier1h = 1.0
	}

	record := &store.EndpointRecord{
		Channel:                       input.Channel,
		Name:                          input.Name,
		URL:                           input.URL,
		Token:                         input.Token,
		ApiKey:                        input.ApiKey,
		Headers:                       input.Headers,
		Priority:                      input.Priority,
		FailoverEnabled:               input.FailoverEnabled,
		CooldownSeconds:               input.CooldownSeconds,
		TimeoutSeconds:                input.TimeoutSeconds,
		SupportsCountTokens:           input.SupportsCountTokens,
		CostMultiplier:                input.CostMultiplier,
		InputCostMultiplier:           input.InputCostMultiplier,
		OutputCostMultiplier:          input.OutputCostMultiplier,
		CacheCreationCostMultiplier:   input.CacheCreationCostMultiplier,
		CacheCreationCostMultiplier1h: input.CacheCreationCostMultiplier1h,
		CacheReadCostMultiplier:       input.CacheReadCostMultiplier,
		Enabled:                       false, // v5.0: 新建端点默认不激活，需手动激活
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// v6.1+: 若渠道表已启用，确保渠道已存在（兼容旧数据：允许端点创建时补齐渠道记录）
	if channelService != nil && input.Channel != "" {
		_ = channelService.EnsureChannel(ctx, input.Channel)
	}

	_, err := endpointService.CreateEndpoint(ctx, record)
	if err != nil {
		return fmt.Errorf("创建端点失败: %w", err)
	}

	if logger != nil {
		logger.Info("✅ 端点已创建", "name", input.Name, "channel", input.Channel)
	}

	// v5.0: endpointService.CreateEndpoint 已经将端点添加到内存并触发健康检测
	// 不需要再次调用 AddEndpoint，否则会导致 "端点已存在" 错误

	// v5.0: 创建成功后，异步同步端点倍率到 UsageTracker
	go a.syncEndpointMultipliersToTracker(context.Background())

	return nil
}

// UpdateEndpointRecord 更新端点
func (a *App) UpdateEndpointRecord(name string, input CreateEndpointInput) error {
	a.mu.RLock()
	endpointService := a.endpointService
	channelService := a.channelService
	usageTracker := a.usageTracker
	storeDB := a.storeDB
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}

	oldName := strings.TrimSpace(name)
	if oldName == "" {
		return fmt.Errorf("端点名称不能为空")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// v6.2+: endpoints 允许不同渠道同名，旧接口仅靠 name 定位会产生歧义。
	// 当检测到 name 在多个渠道重复时，要求前端使用按 ID 更新接口。
	db := storeDB
	if db == nil && usageTracker != nil {
		db = usageTracker.GetDB()
	}
	if db != nil {
		var cnt int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM endpoints WHERE name = ?", oldName).Scan(&cnt); err == nil {
			if cnt > 1 {
				return fmt.Errorf("端点名称 '%s' 在多个渠道中重复，请升级客户端后使用按ID更新", oldName)
			}
		}
	}

	// v5.0: 从数据库获取当前记录，用于保留敏感字段
	existingRecord, err := endpointService.GetEndpoint(ctx, oldName)
	if err != nil {
		return fmt.Errorf("获取端点失败: %w", err)
	}
	if existingRecord == nil {
		return fmt.Errorf("端点 '%s' 不存在", oldName)
	}

	channel := strings.TrimSpace(input.Channel)
	if channel == "" {
		channel = existingRecord.Channel
	}
	newName := strings.TrimSpace(input.Name)
	if newName == "" {
		newName = existingRecord.Name
	}

	// 确保目标渠道存在（避免出现“孤儿渠道/端点”）
	if channelService != nil && channel != "" {
		_ = channelService.EnsureChannel(ctx, channel)
	}

	// 处理 Token: 如果前端传空值，保留原有 Token（防止误删）
	token := input.Token
	if token == "" {
		token = existingRecord.Token
	}

	// 处理 ApiKey: 如果前端传空值，保留原有 ApiKey（防止误删）
	apiKey := input.ApiKey
	if apiKey == "" {
		apiKey = existingRecord.ApiKey
	}

	// 兼容：前端未传 1h 倍率时，保留旧值
	cacheCreationCostMultiplier1h := input.CacheCreationCostMultiplier1h
	if cacheCreationCostMultiplier1h == 0 {
		cacheCreationCostMultiplier1h = existingRecord.CacheCreationCostMultiplier1h
	}

	record := &store.EndpointRecord{
		ID:                            existingRecord.ID,
		Channel:                       channel,
		Name:                          newName,
		URL:                           input.URL,
		Token:                         token,  // 空值时保留原有值
		ApiKey:                        apiKey, // 空值时保留原有值
		Headers:                       input.Headers,
		Priority:                      input.Priority,
		FailoverEnabled:               input.FailoverEnabled,
		CooldownSeconds:               input.CooldownSeconds,
		TimeoutSeconds:                input.TimeoutSeconds,
		SupportsCountTokens:           input.SupportsCountTokens,
		CostMultiplier:                input.CostMultiplier,
		InputCostMultiplier:           input.InputCostMultiplier,
		OutputCostMultiplier:          input.OutputCostMultiplier,
		CacheCreationCostMultiplier:   input.CacheCreationCostMultiplier,
		CacheCreationCostMultiplier1h: cacheCreationCostMultiplier1h,
		CacheReadCostMultiplier:       input.CacheReadCostMultiplier,
		Enabled:                       existingRecord.Enabled, // 保持原有激活状态
	}

	if err := endpointService.UpdateEndpoint(ctx, record); err != nil {
		// 提示更清晰的“同渠道同名”冲突信息
		msg := err.Error()
		// EndpointService 已返回用户可读的冲突原因（避免上层泛化覆盖）
		if strings.Contains(msg, "同一渠道内端点名称必须唯一") {
			return err
		}
		if strings.Contains(msg, "UNIQUE constraint failed: endpoints.channel, endpoints.name") ||
			strings.Contains(msg, "渠道 '") && strings.Contains(msg, " 已存在") {
			return fmt.Errorf("同一渠道 '%s' 下已存在端点名称 '%s'，请修改名称或选择其他渠道", channel, newName)
		}
		return fmt.Errorf("更新端点失败: %w", err)
	}

	if logger != nil {
		if newName == existingRecord.Name && channel == existingRecord.Channel {
			logger.Info("✅ 端点已更新", "name", oldName)
		} else {
			logger.Info("✅ 端点已更新", "from", endpoint.EndpointKey(existingRecord.Channel, existingRecord.Name), "to", endpoint.EndpointKey(channel, newName))
		}
	}

	// best-effort：同步历史记录中的 endpoint_name（仅限“同一渠道内改名”，避免跨渠道误更新）
	if db != nil && existingRecord.Channel == channel && existingRecord.Name != newName {
		_, _ = db.ExecContext(ctx,
			"UPDATE request_logs SET endpoint_name = ? WHERE endpoint_name = ? AND (group_name = ? OR channel = ?)",
			newName, existingRecord.Name, channel, channel,
		)
		_, _ = db.ExecContext(ctx,
			"UPDATE usage_summary SET endpoint_name = ? WHERE endpoint_name = ? AND group_name = ?",
			newName, existingRecord.Name, channel,
		)
	}

	// v5.0: 更新成功后，异步同步端点倍率到 UsageTracker
	// 确保成本计算使用最新的倍率配置
	go a.syncEndpointMultipliersToTracker(context.Background())

	// 推送前端刷新
	a.emitEndpointUpdate()

	return nil
}

// UpdateEndpointRecordByID 更新端点（按ID，避免同名歧义）
func (a *App) UpdateEndpointRecordByID(id int64, input CreateEndpointInput) error {
	a.mu.RLock()
	endpointService := a.endpointService
	channelService := a.channelService
	usageTracker := a.usageTracker
	storeDB := a.storeDB
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}
	if id <= 0 {
		return fmt.Errorf("端点ID不能为空")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	existingRecord, err := endpointService.GetEndpointByID(ctx, id)
	if err != nil {
		return fmt.Errorf("获取端点失败: %w", err)
	}
	if existingRecord == nil {
		return fmt.Errorf("端点不存在: %d", id)
	}

	db := storeDB
	if db == nil && usageTracker != nil {
		db = usageTracker.GetDB()
	}

	channel := strings.TrimSpace(input.Channel)
	if channel == "" {
		channel = existingRecord.Channel
	}
	newName := strings.TrimSpace(input.Name)
	if newName == "" {
		newName = existingRecord.Name
	}

	// 确保目标渠道存在（避免出现“孤儿渠道/端点”）
	if channelService != nil && channel != "" {
		_ = channelService.EnsureChannel(ctx, channel)
	}

	// 处理 Token: 如果前端传空值，保留原有 Token（防止误删）
	token := input.Token
	if token == "" {
		token = existingRecord.Token
	}

	// 处理 ApiKey: 如果前端传空值，保留原有 ApiKey（防止误删）
	apiKey := input.ApiKey
	if apiKey == "" {
		apiKey = existingRecord.ApiKey
	}

	// 兼容：前端未传 1h 倍率时，保留旧值
	cacheCreationCostMultiplier1h := input.CacheCreationCostMultiplier1h
	if cacheCreationCostMultiplier1h == 0 {
		cacheCreationCostMultiplier1h = existingRecord.CacheCreationCostMultiplier1h
	}

	record := &store.EndpointRecord{
		ID:                            existingRecord.ID,
		Channel:                       channel,
		Name:                          newName,
		URL:                           input.URL,
		Token:                         token,  // 空值时保留原有值
		ApiKey:                        apiKey, // 空值时保留原有值
		Headers:                       input.Headers,
		Priority:                      input.Priority,
		FailoverEnabled:               input.FailoverEnabled,
		CooldownSeconds:               input.CooldownSeconds,
		TimeoutSeconds:                input.TimeoutSeconds,
		SupportsCountTokens:           input.SupportsCountTokens,
		CostMultiplier:                input.CostMultiplier,
		InputCostMultiplier:           input.InputCostMultiplier,
		OutputCostMultiplier:          input.OutputCostMultiplier,
		CacheCreationCostMultiplier:   input.CacheCreationCostMultiplier,
		CacheCreationCostMultiplier1h: cacheCreationCostMultiplier1h,
		CacheReadCostMultiplier:       input.CacheReadCostMultiplier,
		Enabled:                       existingRecord.Enabled, // 保持原有激活状态
	}

	if err := endpointService.UpdateEndpoint(ctx, record); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "同一渠道内端点名称必须唯一") {
			return err
		}
		if strings.Contains(msg, "UNIQUE constraint failed: endpoints.channel, endpoints.name") ||
			(strings.Contains(msg, "渠道 '") && strings.Contains(msg, " 已存在")) {
			return fmt.Errorf("同一渠道 '%s' 下已存在端点名称 '%s'，请修改名称或选择其他渠道", channel, newName)
		}
		return fmt.Errorf("更新端点失败: %w", err)
	}

	if logger != nil {
		if newName == existingRecord.Name && channel == existingRecord.Channel {
			logger.Info("✅ 端点已更新", "id", id, "name", newName)
		} else {
			logger.Info("✅ 端点已更新", "id", id, "from", endpoint.EndpointKey(existingRecord.Channel, existingRecord.Name), "to", endpoint.EndpointKey(channel, newName))
		}
	}

	// best-effort：同步历史记录中的 endpoint_name（仅限“同一渠道内改名”，避免跨渠道误更新）
	if db != nil && existingRecord.Channel == channel && existingRecord.Name != newName {
		_, _ = db.ExecContext(ctx,
			"UPDATE request_logs SET endpoint_name = ? WHERE endpoint_name = ? AND (group_name = ? OR channel = ?)",
			newName, existingRecord.Name, channel, channel,
		)
		_, _ = db.ExecContext(ctx,
			"UPDATE usage_summary SET endpoint_name = ? WHERE endpoint_name = ? AND group_name = ?",
			newName, existingRecord.Name, channel,
		)
	}

	go a.syncEndpointMultipliersToTracker(context.Background())
	a.emitEndpointUpdate()

	return nil
}

// SetEndpointFailoverEnabled 设置端点是否参与故障转移（快捷开关）
// 仅影响当前端点所在渠道内的候选端点集合。
func (a *App) SetEndpointFailoverEnabled(name string, enabled bool) error {
	a.mu.RLock()
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	record, err := endpointService.GetEndpoint(ctx, name)
	if err != nil {
		return fmt.Errorf("获取端点失败: %w", err)
	}
	if record == nil {
		return fmt.Errorf("端点 '%s' 不存在", name)
	}

	record.FailoverEnabled = enabled
	if err := endpointService.UpdateEndpoint(ctx, record); err != nil {
		return fmt.Errorf("更新端点失败: %w", err)
	}

	if logger != nil {
		logger.Info("✅ 端点故障转移参与状态已更新", "name", name, "enabled", enabled)
	}

	return nil
}

// SetEndpointFailoverEnabledByID 设置端点是否参与故障转移（按ID，避免同名歧义）
func (a *App) SetEndpointFailoverEnabledByID(id int64, enabled bool) error {
	a.mu.RLock()
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}
	if id <= 0 {
		return fmt.Errorf("端点ID不能为空")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	record, err := endpointService.GetEndpointByID(ctx, id)
	if err != nil {
		return fmt.Errorf("获取端点失败: %w", err)
	}
	if record == nil {
		return fmt.Errorf("端点不存在: %d", id)
	}

	record.FailoverEnabled = enabled
	if err := endpointService.UpdateEndpoint(ctx, record); err != nil {
		return fmt.Errorf("更新端点失败: %w", err)
	}

	if logger != nil {
		logger.Info("✅ 端点故障转移参与状态已更新", "id", id, "name", record.Name, "enabled", enabled)
	}

	a.emitEndpointUpdate()
	return nil
}

// DeleteEndpointRecord 删除端点
func (a *App) DeleteEndpointRecord(name string) error {
	a.mu.RLock()
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := endpointService.DeleteEndpoint(ctx, name); err != nil {
		return fmt.Errorf("删除端点失败: %w", err)
	}

	if logger != nil {
		logger.Info("✅ 端点已删除", "name", name)
	}

	// v5.0: 删除成功后，异步同步端点倍率到 UsageTracker
	go a.syncEndpointMultipliersToTracker(context.Background())

	return nil
}

// DeleteEndpointRecordByID 删除端点（按ID，避免同名歧义）
func (a *App) DeleteEndpointRecordByID(id int64) error {
	a.mu.RLock()
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}
	if id <= 0 {
		return fmt.Errorf("端点ID不能为空")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := endpointService.DeleteEndpointByID(ctx, id); err != nil {
		return fmt.Errorf("删除端点失败: %w", err)
	}

	if logger != nil {
		logger.Info("✅ 端点已删除", "id", id)
	}

	go a.syncEndpointMultipliersToTracker(context.Background())
	a.emitEndpointUpdate()
	return nil
}

// ToggleEndpointRecord 切换端点启用状态
// v6.0: 切换语义升级为“激活/停用渠道(channel)”
func (a *App) ToggleEndpointRecord(name string, enabled bool) error {
	a.mu.RLock()
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if enabled {
		// 激活：以端点所属渠道为单位
		record, err := endpointService.GetEndpoint(ctx, name)
		if err != nil {
			return fmt.Errorf("获取端点配置失败: %w", err)
		}
		if record == nil {
			return fmt.Errorf("端点 '%s' 不存在", name)
		}
		if err := endpointService.ActivateChannel(ctx, record.Channel); err != nil {
			return fmt.Errorf("激活渠道失败: %w", err)
		}
	} else {
		// 停用：以端点所属渠道为单位
		record, err := endpointService.GetEndpoint(ctx, name)
		if err != nil {
			return fmt.Errorf("获取端点配置失败: %w", err)
		}
		if record == nil {
			return fmt.Errorf("端点 '%s' 不存在", name)
		}
		if err := endpointService.DeactivateChannel(ctx, record.Channel); err != nil {
			return fmt.Errorf("停用渠道失败: %w", err)
		}
	}

	status := map[bool]string{true: "启用", false: "禁用"}[enabled]

	if logger != nil {
		logger.Info("✅ 端点状态已切换", "name", name, "status", status)
	}

	return nil
}

// ToggleEndpointRecordByID 切换端点启用状态（按ID，避免同名歧义）
// v6.0: 切换语义升级为“激活/停用渠道(channel)”
func (a *App) ToggleEndpointRecordByID(id int64, enabled bool) error {
	a.mu.RLock()
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if endpointService == nil {
		return fmt.Errorf("端点存储服务未启用")
	}
	if id <= 0 {
		return fmt.Errorf("端点ID不能为空")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	record, err := endpointService.GetEndpointByID(ctx, id)
	if err != nil {
		return fmt.Errorf("获取端点配置失败: %w", err)
	}
	if record == nil {
		return fmt.Errorf("端点不存在: %d", id)
	}

	if enabled {
		if err := endpointService.ActivateChannel(ctx, record.Channel); err != nil {
			return fmt.Errorf("激活渠道失败: %w", err)
		}
	} else {
		if err := endpointService.DeactivateChannel(ctx, record.Channel); err != nil {
			return fmt.Errorf("停用渠道失败: %w", err)
		}
	}

	status := map[bool]string{true: "启用", false: "禁用"}[enabled]
	if logger != nil {
		logger.Info("✅ 端点状态已切换", "id", id, "channel", record.Channel, "name", record.Name, "status", status)
	}

	a.emitEndpointUpdate()
	return nil
}

// ChannelInfo 渠道信息
type ChannelInfo struct {
	Name          string `json:"name"`
	Website       string `json:"website,omitempty"`
	Priority      int    `json:"priority"`
	CreatedAt     string `json:"created_at"`
	EndpointCount int    `json:"endpoint_count"`
}

// GetChannels 获取所有渠道
func (a *App) GetChannels() ([]ChannelInfo, error) {
	a.mu.RLock()
	endpointService := a.endpointService
	channelService := a.channelService
	usageTracker := a.usageTracker
	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	channelCount := make(map[string]int)
	// 统计每个渠道的端点数（兼容：端点表中存在但 channels 表未收录的渠道）
	// 优先用 EndpointService（字段最全）；若因旧库缺字段导致失败，则降级为聚合 SQL（只依赖 channel 列）
	if endpointService != nil {
		records, err := endpointService.ListEndpoints(ctx)
		if err == nil {
			for _, r := range records {
				if r.Channel == "" {
					continue
				}
				channelCount[r.Channel]++
			}
		} else if usageTracker != nil && usageTracker.GetDB() != nil {
			rows, qerr := usageTracker.GetDB().QueryContext(ctx, "SELECT channel, COUNT(*) FROM endpoints GROUP BY channel")
			if qerr == nil {
				defer rows.Close()
				for rows.Next() {
					var name string
					var count int
					if err := rows.Scan(&name, &count); err != nil {
						continue
					}
					if name == "" {
						continue
					}
					channelCount[name] = count
				}
			}
		}
	}

	channelWebsite := make(map[string]string)
	channelPriority := make(map[string]int)
	channelCreatedAt := make(map[string]string)
	orderedChannels := make([]string, 0, 16)
	if channelService != nil {
		channelRecords, err := channelService.ListChannels(ctx)
		if err != nil {
			return nil, fmt.Errorf("获取渠道列表失败: %w", err)
		}
		for _, c := range channelRecords {
			if c == nil || c.Name == "" {
				continue
			}
			channelWebsite[c.Name] = c.Website
			if c.Priority <= 0 {
				channelPriority[c.Name] = 1
			} else {
				channelPriority[c.Name] = c.Priority
			}
			if !c.CreatedAt.IsZero() {
				channelCreatedAt[c.Name] = c.CreatedAt.Format("2006-01-02 15:04:05")
			}
			if _, ok := channelCount[c.Name]; !ok {
				channelCount[c.Name] = 0
			}
			orderedChannels = append(orderedChannels, c.Name)
		}
	}

	seen := make(map[string]struct{}, len(channelCount))
	result := make([]ChannelInfo, 0, len(channelCount))

	// 1) channels 表中的渠道（已按 store 层排序），优先输出，保证 UI 顺序稳定
	for _, name := range orderedChannels {
		count := channelCount[name]
		result = append(result, ChannelInfo{
			Name:          name,
			Website:       channelWebsite[name],
			Priority:      channelPriority[name],
			CreatedAt:     channelCreatedAt[name],
			EndpointCount: count,
		})
		seen[name] = struct{}{}
	}

	// 2) 兼容：端点表里存在但 channels 表未收录的渠道（作为兜底）
	for name, count := range channelCount {
		if _, ok := seen[name]; ok {
			continue
		}
		result = append(result, ChannelInfo{
			Name:          name,
			Website:       channelWebsite[name],
			Priority:      999,
			CreatedAt:     "",
			EndpointCount: count,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		pi := result[i].Priority
		pj := result[j].Priority
		if pi <= 0 {
			pi = 1
		}
		if pj <= 0 {
			pj = 1
		}
		if pi != pj {
			return pi < pj
		}
		// 同优先级：按创建时间倒序（新创建排在前面）
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt > result[j].CreatedAt
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

type CreateChannelInput struct {
	Name     string `json:"name"`
	Website  string `json:"website,omitempty"`
	Priority int    `json:"priority"`
}

func (a *App) CreateChannel(input CreateChannelInput) error {
	a.mu.RLock()
	channelService := a.channelService
	endpointManager := a.endpointManager
	logger := a.logger
	a.mu.RUnlock()

	if channelService == nil {
		return fmt.Errorf("渠道存储服务未启用")
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Website = strings.TrimSpace(input.Website)
	if input.Name == "" {
		return fmt.Errorf("渠道名称不能为空")
	}
	if input.Priority <= 0 {
		input.Priority = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := channelService.CreateChannel(ctx, &store.ChannelRecord{
		Name:     input.Name,
		Website:  input.Website,
		Priority: input.Priority,
	})
	if err != nil {
		return err
	}
	if logger != nil {
		logger.Info("✅ 渠道已创建", "name", input.Name)
	}

	// 同步渠道优先级到运行时（用于渠道间故障转移顺序）
	if endpointManager != nil {
		a.syncChannelPrioritiesToEndpointManager(ctx)
	}
	return nil
}

type UpdateChannelInput struct {
	Name     string `json:"name"`
	Website  string `json:"website,omitempty"`
	Priority int    `json:"priority"`
}

func (a *App) UpdateChannel(input UpdateChannelInput) error {
	a.mu.RLock()
	channelService := a.channelService
	endpointManager := a.endpointManager
	logger := a.logger
	a.mu.RUnlock()

	if channelService == nil {
		return fmt.Errorf("渠道存储服务未启用")
	}

	input.Name = strings.TrimSpace(input.Name)
	input.Website = strings.TrimSpace(input.Website)
	if input.Name == "" {
		return fmt.Errorf("渠道名称不能为空")
	}
	if input.Priority <= 0 {
		input.Priority = 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := channelService.UpdateChannel(ctx, &store.ChannelRecord{
		Name:     input.Name,
		Website:  input.Website,
		Priority: input.Priority,
	}); err != nil {
		return err
	}

	if endpointManager != nil {
		a.syncChannelPrioritiesToEndpointManager(ctx)
	}

	// 推送前端刷新（channelsMeta & groups）
	a.emitEndpointUpdate()

	if logger != nil {
		logger.Info("✅ 渠道已更新", "name", input.Name, "priority", input.Priority)
	}
	return nil
}

func (a *App) syncChannelPrioritiesToEndpointManager(ctx context.Context) {
	a.mu.RLock()
	channelStore := a.channelStore
	endpointManager := a.endpointManager
	logger := a.logger
	a.mu.RUnlock()

	if channelStore == nil || endpointManager == nil {
		return
	}

	baseCtx := ctx
	if baseCtx == nil || baseCtx.Err() != nil {
		baseCtx = context.Background()
	}

	syncCtx, cancel := context.WithTimeout(baseCtx, 3*time.Second)
	defer cancel()

	records, err := channelStore.List(syncCtx)
	if err != nil {
		if logger != nil {
			logger.Warn("⚠️ 同步渠道优先级失败", "error", err)
		}
		return
	}

	priorities := make(map[string]int, len(records))
	for _, r := range records {
		if r == nil || r.Name == "" {
			continue
		}
		p := r.Priority
		if p <= 0 {
			p = 1
		}
		priorities[r.Name] = p
	}
	endpointManager.UpdateChannelPriorities(priorities)
}

// DeleteChannel 删除渠道
// deleteEndpoints=true 时会一并删除该渠道下所有端点。
func (a *App) DeleteChannel(name string, deleteEndpoints bool) error {
	a.mu.RLock()
	channelStore := a.channelStore
	channelService := a.channelService
	endpointStore := a.endpointStore
	endpointService := a.endpointService
	usageTracker := a.usageTracker
	storeDB := a.storeDB
	logger := a.logger
	a.mu.RUnlock()

	if channelService == nil || channelStore == nil {
		return fmt.Errorf("渠道存储服务未启用")
	}
	if usageTracker == nil || usageTracker.GetDB() == nil {
		if storeDB == nil {
			return fmt.Errorf("数据库未初始化")
		}
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("渠道名称不能为空")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	db := storeDB
	if db == nil && usageTracker != nil {
		db = usageTracker.GetDB()
	}
	if db == nil {
		return fmt.Errorf("数据库未初始化")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if endpointStore == nil {
		endpointStore = store.NewSQLiteEndpointStore(db)
	}
	endpointStore = endpointStore.WithTx(tx)
	channelStore = channelStore.WithTx(tx)

	// 端点检查/删除
	endpoints, err := endpointStore.ListByChannel(ctx, name)
	if err != nil {
		return fmt.Errorf("获取渠道端点失败: %w", err)
	}
	if len(endpoints) > 0 && !deleteEndpoints {
		return fmt.Errorf("渠道 '%s' 下仍有 %d 个端点，请勾选一并删除", name, len(endpoints))
	}
	if len(endpoints) > 0 {
		names := make([]string, 0, len(endpoints))
		for _, ep := range endpoints {
			if ep != nil && ep.Name != "" {
				names = append(names, ep.Name)
			}
		}
		if len(names) > 0 {
			if err := endpointStore.BatchDelete(ctx, names); err != nil {
				return fmt.Errorf("删除渠道端点失败: %w", err)
			}
		}
	}

	// 删除渠道
	if err := channelStore.Delete(ctx, name); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}

	// 同步运行时配置（删除端点/组）
	if endpointService != nil {
		if err := endpointService.SyncFromDatabase(ctx); err != nil && logger != nil {
			logger.Warn("⚠️ 删除渠道后同步运行时配置失败", "error", err)
		}
	}

	// 推送前端刷新
	a.emitEndpointUpdate()

	if logger != nil {
		logger.Info("✅ 渠道已删除", "name", name, "delete_endpoints", deleteEndpoints)
	}

	return nil
}

// GetEndpointsByChannel 按渠道获取端点
func (a *App) GetEndpointsByChannel(channel string) ([]EndpointRecordInfo, error) {
	a.mu.RLock()
	endpointService := a.endpointService
	endpointManager := a.endpointManager
	a.mu.RUnlock()

	if endpointService == nil {
		return nil, fmt.Errorf("端点存储服务未启用")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	records, err := endpointService.ListEndpointsByChannel(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("获取渠道端点列表失败: %w", err)
	}

	result := make([]EndpointRecordInfo, 0, len(records))
	for _, r := range records {
		info := a.recordToInfo(r)

		// 获取运行时健康状态
		if endpointManager != nil {
			status := endpointManager.GetEndpointStatus(r.Name)
			info.Healthy = status.Healthy
			info.ResponseTimeMs = float64(status.ResponseTime.Milliseconds())
			// 格式化最后检查时间
			if !status.LastCheck.IsZero() {
				info.LastCheck = status.LastCheck.Format("2006-01-02 15:04:05")
			}
			// 冷却状态
			if !status.CooldownUntil.IsZero() && status.CooldownUntil.After(time.Now()) {
				info.InCooldown = true
				info.CooldownUntil = status.CooldownUntil.Format("2006-01-02 15:04:05")
				info.CooldownReason = status.CooldownReason
			}
		}

		result = append(result, info)
	}

	return result, nil
}

// recordToInfo 将数据库记录转换为前端 Info 结构
func (a *App) recordToInfo(r *store.EndpointRecord) EndpointRecordInfo {
	info := EndpointRecordInfo{
		ID:                          r.ID,
		Channel:                     r.Channel,
		Name:                        r.Name,
		URL:                         r.URL,
		Token:                       r.Token,  // v5.0: 本地桌面应用，直接返回原始 Token
		ApiKey:                      r.ApiKey, // v5.0: 本地桌面应用，直接返回原始 ApiKey
		TokenMasked:                 maskToken(r.Token),
		ApiKeyMasked:                maskToken(r.ApiKey),
		Headers:                     r.Headers,
		Priority:                    r.Priority,
		FailoverEnabled:             r.FailoverEnabled,
		CooldownSeconds:             r.CooldownSeconds,
		TimeoutSeconds:              r.TimeoutSeconds,
		SupportsCountTokens:         r.SupportsCountTokens,
		CostMultiplier:              r.CostMultiplier,
		InputCostMultiplier:         r.InputCostMultiplier,
		OutputCostMultiplier:        r.OutputCostMultiplier,
		CacheCreationCostMultiplier: r.CacheCreationCostMultiplier,
		CacheReadCostMultiplier:     r.CacheReadCostMultiplier,
		Enabled:                     r.Enabled,
	}

	if !r.CreatedAt.IsZero() {
		info.CreatedAt = r.CreatedAt.Format("2006-01-02 15:04:05")
	}
	if !r.UpdatedAt.IsZero() {
		info.UpdatedAt = r.UpdatedAt.Format("2006-01-02 15:04:05")
	}

	return info
}

// maskToken Token 脱敏显示
func maskToken(token string) string {
	if token == "" {
		return "" // 空 token 返回空字符串，不显示"已配置"
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
