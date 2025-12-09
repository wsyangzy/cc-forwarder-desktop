package endpoint

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"cc-forwarder/config"
	"cc-forwarder/internal/events"
	"cc-forwarder/internal/transport"
	"cc-forwarder/internal/utils"
)

// EndpointStatus represents the health status of an endpoint
type EndpointStatus struct {
	Healthy         bool
	LastCheck       time.Time
	ResponseTime    time.Duration
	ConsecutiveFails int
	NeverChecked    bool  // 表示从未被检测过
}

// Endpoint represents an endpoint with its configuration and status
type Endpoint struct {
	Config config.EndpointConfig
	Status EndpointStatus
	mutex  sync.RWMutex
}

// Manager manages endpoints and their health status
type Manager struct {
	endpoints    []*Endpoint
	endpointsMu  sync.RWMutex  // v5.0+: 保护 endpoints 切片的并发访问
	config       *config.Config
	client       *http.Client
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	fastTester   *FastTester
	groupManager *GroupManager
	keyManager   *KeyManager // 管理多 API Key 状态
	// EventBus for decoupled event publishing
	eventBus     events.EventBus
	// 健康检查完成回调（用于推送 Wails 事件）
	onHealthCheckComplete func()
}


// NewManager creates a new endpoint manager
func NewManager(cfg *config.Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	
	// Create transport with proxy support
	httpTransport, err := transport.CreateTransport(cfg)
	if err != nil {
		slog.Error(fmt.Sprintf("❌ Failed to create HTTP transport with proxy: %s", err.Error()))
		// Fall back to default transport
		httpTransport = &http.Transport{}
	}
	
	
	manager := &Manager{
		config:       cfg,
		client: &http.Client{
			Timeout:   cfg.Health.Timeout,
			Transport: httpTransport,
		},
		ctx:          ctx,
		cancel:       cancel,
		fastTester:   NewFastTester(cfg),
		groupManager: NewGroupManager(cfg),
		keyManager:   NewKeyManager(), // 初始化 Key 管理器
	}

	// Initialize endpoints
	for _, endpointCfg := range cfg.Endpoints {
		endpoint := &Endpoint{
			Config: endpointCfg,
			Status: EndpointStatus{
				Healthy:      false, // Start pessimistic, let health checks determine actual status
				LastCheck:    time.Now(),
				NeverChecked: true,  // 标记为未检测
			},
		}
		manager.endpoints = append(manager.endpoints, endpoint)

		// 初始化端点的 Key 状态
		tokenCount := len(endpointCfg.Tokens)
		if tokenCount == 0 && endpointCfg.Token != "" {
			tokenCount = 1 // 单 Token 算作 1 个
		}
		apiKeyCount := len(endpointCfg.ApiKeys)
		if apiKeyCount == 0 && endpointCfg.ApiKey != "" {
			apiKeyCount = 1 // 单 API Key 算作 1 个
		}
		manager.keyManager.InitEndpoint(endpointCfg.Name, tokenCount, apiKeyCount)
	}

	// Set manager reference in fast tester for dynamic token resolution
	manager.fastTester.SetManager(manager)

	// Initialize groups from endpoints
	manager.groupManager.UpdateGroups(manager.endpoints)

	return manager
}

// Start starts the health checking routine
func (m *Manager) Start() {
	m.wg.Add(1)
	go m.healthCheckLoop()
}

// Stop stops the health checking routine
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// UpdateConfig updates the manager configuration (hot-reload)
// v5.0 Desktop: 只更新配置参数，不重建端点（端点完全由数据库管理）
func (m *Manager) UpdateConfig(cfg *config.Config) {
	m.config = cfg

	// 只更新 GroupManager 配置
	m.groupManager.UpdateConfig(cfg)
	slog.Debug("🔄 [热更新] 更新配置参数完成，端点保持不变")
	
	// Update fast tester with new config
	if m.fastTester != nil {
		m.fastTester.UpdateConfig(cfg)
	}
	
	// Recreate transport with new proxy configuration
	if transport, err := transport.CreateTransport(cfg); err == nil {
		m.client = &http.Client{
			Transport: transport,
			Timeout:   cfg.Health.Timeout,
		}
	}
}

// GetHealthyEndpoints returns a list of healthy endpoints from active groups based on strategy
func (m *Manager) GetHealthyEndpoints() []*Endpoint {
	// v5.0+: 使用快照机制
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// First filter by active groups
	// v5.0: SQLite 模式下，enabled=true ⇔ group.IsActive=true（已同步）
	activeEndpoints := m.groupManager.FilterEndpointsByActiveGroups(snapshot)

	// Then filter by health status
	var healthy []*Endpoint
	for _, endpoint := range activeEndpoints {
		endpoint.mutex.RLock()
		if endpoint.Status.Healthy {
			healthy = append(healthy, endpoint)
		}
		endpoint.mutex.RUnlock()
	}

	return m.sortHealthyEndpoints(healthy, true) // Show logs by default
}

// sortHealthyEndpoints sorts healthy endpoints based on strategy with optional logging
func (m *Manager) sortHealthyEndpoints(healthy []*Endpoint, showLogs bool) []*Endpoint {
	// Sort based on strategy
	switch m.config.Strategy.Type {
	case "priority":
		sort.Slice(healthy, func(i, j int) bool {
			return healthy[i].Config.Priority < healthy[j].Config.Priority
		})
	case "fastest":
		// Log endpoint latencies for fastest strategy (only if showLogs is true)
		if len(healthy) > 1 && showLogs {
			slog.Info("📊 [Fastest Strategy] 基于健康检查的端点延迟排序:")
			for _, ep := range healthy {
				ep.mutex.RLock()
				responseTime := ep.Status.ResponseTime
				ep.mutex.RUnlock()
				slog.Info(fmt.Sprintf("  ⏱️ %s - 延迟: %dms (来源: 定期健康检查)", 
					ep.Config.Name, responseTime.Milliseconds()))
			}
		}
		
		sort.Slice(healthy, func(i, j int) bool {
			healthy[i].mutex.RLock()
			healthy[j].mutex.RLock()
			defer healthy[i].mutex.RUnlock()
			defer healthy[j].mutex.RUnlock()
			return healthy[i].Status.ResponseTime < healthy[j].Status.ResponseTime
		})
	}

	return healthy
}

// GetFastestEndpointsWithRealTimeTest returns endpoints from active groups sorted by real-time testing
func (m *Manager) GetFastestEndpointsWithRealTimeTest(ctx context.Context) []*Endpoint {
	// v5.0+: 使用快照机制
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// First get endpoints from active groups and filter by health
	activeEndpoints := m.groupManager.FilterEndpointsByActiveGroups(snapshot)
	
	var healthy []*Endpoint
	for _, endpoint := range activeEndpoints {
		endpoint.mutex.RLock()
		if endpoint.Status.Healthy {
			healthy = append(healthy, endpoint)
		}
		endpoint.mutex.RUnlock()
	}
	
	if len(healthy) == 0 {
		return healthy
	}

	// If not using fastest strategy or fast test disabled, apply sorting with logging
	if m.config.Strategy.Type != "fastest" || !m.config.Strategy.FastTestEnabled {
		return m.sortHealthyEndpoints(healthy, true) // Show logs
	}

	// Check if we have cached fast test results first
	testResults, usedCache := m.fastTester.TestEndpointsParallel(ctx, healthy)
	
	// Only show health check sorting if we're NOT using cache
	if !usedCache && m.config.Strategy.Type == "fastest" && len(healthy) > 1 {
		slog.InfoContext(ctx, "📊 [Fastest Strategy] 基于健康检查的活跃组端点延迟排序:")
		for _, ep := range healthy {
			ep.mutex.RLock()
			responseTime := ep.Status.ResponseTime
			group := ep.Config.Group
			ep.mutex.RUnlock()
			slog.InfoContext(ctx, fmt.Sprintf("  ⏱️ %s (组: %s) - 延迟: %dms (来源: 定期健康检查)", 
				ep.Config.Name, group, responseTime.Milliseconds()))
		}
	}
	
	// Log ALL test results first (including failures) - but only if cache wasn't used
	if len(testResults) > 0 && !usedCache {
		slog.InfoContext(ctx, "🔍 [Fastest Response Mode] 活跃组端点性能测试结果:")
		successCount := 0
		for _, result := range testResults {
			group := result.Endpoint.Config.Group
			if result.Success {
				successCount++
				slog.InfoContext(ctx, fmt.Sprintf("  ✅ 健康 %s (组: %s) - 响应时间: %dms", 
					result.Endpoint.Config.Name, group,
					result.ResponseTime.Milliseconds()))
			} else {
				errorMsg := ""
				if result.Error != nil {
					errorMsg = fmt.Sprintf(" - 错误: %s", result.Error.Error())
				}
				slog.InfoContext(ctx, fmt.Sprintf("  ❌ 异常 %s (组: %s) - 响应时间: %dms%s", 
					result.Endpoint.Config.Name, group,
					result.ResponseTime.Milliseconds(),
					errorMsg))
			}
		}
		
		slog.InfoContext(ctx, fmt.Sprintf("📊 [测试摘要] 活跃组测试: %d个端点, 健康: %d个, 异常: %d个",
			len(testResults), successCount, len(testResults)-successCount))
	}
	
	// Sort by response time (only successful results)
	sortedResults := SortByResponseTime(testResults)
	
	if len(sortedResults) == 0 {
		slog.WarnContext(ctx, "⚠️ [Fastest Response Mode] 活跃组所有端点测试失败，回退到健康检查模式")
		return healthy // Fall back to health check results if no fast tests succeeded
	}
	
	// Convert back to endpoint slice
	endpoints := make([]*Endpoint, 0, len(sortedResults))
	for _, result := range sortedResults {
		endpoints = append(endpoints, result.Endpoint)
	}

	// Log the successful endpoint ranking
	if len(endpoints) > 0 {
		// Show the fastest endpoint selection
		fastestEndpoint := endpoints[0]
		var fastestTime int64
		for _, result := range sortedResults {
			if result.Endpoint == fastestEndpoint {
				fastestTime = result.ResponseTime.Milliseconds()
				break
			}
		}
		
		cacheIndicator := ""
		if usedCache {
			cacheIndicator = " (缓存)"
		}
		
		slog.InfoContext(ctx, fmt.Sprintf("🚀 [Fastest Response Mode] 选择最快端点: %s - %dms%s", 
			fastestEndpoint.Config.Name, fastestTime, cacheIndicator))
		
		// Show other available endpoints if there are more than one
		if len(endpoints) > 1 && !usedCache {
			slog.InfoContext(ctx, "📋 [备用端点] 其他可用端点:")
			for i := 1; i < len(endpoints); i++ {
				ep := endpoints[i]
				var responseTime int64
				var epGroup string
				for _, result := range sortedResults {
					if result.Endpoint == ep {
						responseTime = result.ResponseTime.Milliseconds()
						epGroup = result.Endpoint.Config.Group
						break
					}
				}
				slog.InfoContext(ctx, fmt.Sprintf("  🔄 备用 %s (组: %s) - 响应时间: %dms", 
					ep.Config.Name, epGroup, responseTime))
			}
		}
	}

	return endpoints
}

// GetEndpointByName returns an endpoint by name, only from active groups
func (m *Manager) GetEndpointByName(name string) *Endpoint {
	// v5.0+: 使用快照机制
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// First filter by active groups
	activeEndpoints := m.groupManager.FilterEndpointsByActiveGroups(snapshot)

	// Then find by name
	for _, endpoint := range activeEndpoints {
		if endpoint.Config.Name == name {
			return endpoint
		}
	}
	return nil
}

// GetEndpointByNameAny returns an endpoint by name from all endpoints (ignoring group status)
func (m *Manager) GetEndpointByNameAny(name string) *Endpoint {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	for _, endpoint := range m.endpoints {
		if endpoint.Config.Name == name {
			return endpoint
		}
	}
	return nil
}

// GetAllEndpoints returns all endpoints (deprecated: use GetEndpoints instead)
func (m *Manager) GetAllEndpoints() []*Endpoint {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	result := make([]*Endpoint, len(m.endpoints))
	copy(result, m.endpoints)
	return result
}

// GetTokenForEndpoint dynamically resolves the token for an endpoint
// If the endpoint has its own token, return it
// If not, find the first endpoint in the same group that has a token
// 支持多 Token 配置：优先使用 tokens 数组中当前激活的 Token
func (m *Manager) GetTokenForEndpoint(ep *Endpoint) string {
	// 1. 优先使用多 Tokens 配置（端点独立管理）
	if len(ep.Config.Tokens) > 0 {
		activeIndex := m.keyManager.GetActiveTokenIndex(ep.Config.Name)
		if activeIndex >= 0 && activeIndex < len(ep.Config.Tokens) {
			return ep.Config.Tokens[activeIndex].Value
		}
		return ep.Config.Tokens[0].Value // 回退到第一个
	}

	// 2. 使用单 Token 配置
	if ep.Config.Token != "" {
		return ep.Config.Token
	}

	// 3. 组内继承（仅对单 Token 保持原有行为，多 Token 不继承）
	groupName := ep.Config.Group
	if groupName == "" {
		groupName = "Default"
	}

	// v5.0+: 使用读锁遍历 endpoints
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	// Search through all endpoints for the same group
	for _, endpoint := range m.endpoints {
		endpointGroup := endpoint.Config.Group
		if endpointGroup == "" {
			endpointGroup = "Default"
		}

		// If same group and has token (only single token inheritance)
		if endpointGroup == groupName && endpoint.Config.Token != "" {
			return endpoint.Config.Token
		}
	}

	// 4. No token found in the group
	return ""
}

// GetApiKeyForEndpoint dynamically resolves the API key for an endpoint
// If the endpoint has its own api-key, return it
// If not, find the first endpoint in the same group that has an api-key
// 支持多 API Key 配置：优先使用 api-keys 数组中当前激活的 API Key
func (m *Manager) GetApiKeyForEndpoint(ep *Endpoint) string {
	// 1. 优先使用多 ApiKeys 配置（端点独立管理）
	if len(ep.Config.ApiKeys) > 0 {
		activeIndex := m.keyManager.GetActiveApiKeyIndex(ep.Config.Name)
		if activeIndex >= 0 && activeIndex < len(ep.Config.ApiKeys) {
			return ep.Config.ApiKeys[activeIndex].Value
		}
		return ep.Config.ApiKeys[0].Value // 回退到第一个
	}

	// 2. 使用单 ApiKey 配置
	if ep.Config.ApiKey != "" {
		return ep.Config.ApiKey
	}

	// 3. 组内继承（仅对单 ApiKey 保持原有行为，多 ApiKey 不继承）
	groupName := ep.Config.Group
	if groupName == "" {
		groupName = "Default"
	}

	// v5.0+: 使用读锁遍历 endpoints
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	// Search through all endpoints for the same group
	for _, endpoint := range m.endpoints {
		endpointGroup := endpoint.Config.Group
		if endpointGroup == "" {
			endpointGroup = "Default"
		}

		// If same group and has api-key (only single api-key inheritance)
		if endpointGroup == groupName && endpoint.Config.ApiKey != "" {
			return endpoint.Config.ApiKey
		}
	}

	// 4. No api-key found in the group
	return ""
}

// GetConfig returns the manager's configuration
func (m *Manager) GetConfig() *config.Config {
	return m.config
}

// GetGroupManager returns the group manager
func (m *Manager) GetGroupManager() *GroupManager {
	return m.groupManager
}


// SetEventBus 设置EventBus事件总线
func (m *Manager) SetEventBus(eventBus events.EventBus) {
	m.eventBus = eventBus
}

// SetOnHealthCheckComplete 设置健康检查完成回调
// 用于 Wails 桌面应用在定时健康检查完成后推送事件到前端
func (m *Manager) SetOnHealthCheckComplete(fn func()) {
	m.onHealthCheckComplete = fn
}

// notifyWebInterface 通过EventBus发布端点状态变化事件
func (m *Manager) notifyWebInterface(endpoint *Endpoint) {
	if m.eventBus == nil {
		return
	}
	
	endpoint.mutex.RLock()
	status := endpoint.Status
	endpoint.mutex.RUnlock()
	
	// 确定事件类型和优先级
	eventType := events.EventEndpointHealthy
	priority := events.PriorityHigh
	changeType := "status_changed"
	
	if !status.Healthy {
		eventType = events.EventEndpointUnhealthy
		priority = events.PriorityCritical
		changeType = "health_changed"
	}
	
	m.eventBus.Publish(events.Event{
		Type:     eventType,
		Source:   "endpoint_manager",
		Priority: priority,
		Data: map[string]interface{}{
			"endpoint":        endpoint.Config.Name,
			"healthy":         status.Healthy,
			"response_time":   utils.FormatResponseTime(status.ResponseTime),
			"last_check":      status.LastCheck.Format("2006-01-02 15:04:05"),
			"consecutive_fails": status.ConsecutiveFails,
			"change_type":     changeType,
		},
	})
}

// ManualActivateGroup manually activates a specific group via web interface
func (m *Manager) ManualActivateGroup(groupName string) error {
	err := m.groupManager.ManualActivateGroup(groupName)
	if err != nil {
		return err
	}

	// Notify web interface about group change
	go m.notifyWebGroupChange("group_manually_activated", groupName)

	return nil
}

// ManualActivateGroupWithForce manually activates a specific group via web interface with force option
func (m *Manager) ManualActivateGroupWithForce(groupName string, force bool) error {
	err := m.groupManager.ManualActivateGroupWithForce(groupName, force)
	if err != nil {
		return err
	}

	// Notify web interface about group change
	if force {
		go m.notifyWebGroupChange("group_force_activated", groupName)
	} else {
		go m.notifyWebGroupChange("group_manually_activated", groupName)
	}

	return nil
}

// ManualPauseGroup manually pauses a group via web interface
func (m *Manager) ManualPauseGroup(groupName string, duration time.Duration) error {
	err := m.groupManager.ManualPauseGroup(groupName, duration)
	if err != nil {
		return err
	}
	
	// Notify web interface about group change
	go m.notifyWebGroupChange("group_manually_paused", groupName)
	
	return nil
}

// ManualResumeGroup manually resumes a paused group via web interface
func (m *Manager) ManualResumeGroup(groupName string) error {
	err := m.groupManager.ManualResumeGroup(groupName)
	if err != nil {
		return err
	}
	
	// Notify web interface about group change
	go m.notifyWebGroupChange("group_manually_resumed", groupName)
	
	return nil
}

// GetGroupDetails returns detailed information about all groups for web interface
func (m *Manager) GetGroupDetails() map[string]interface{} {
	return m.groupManager.GetGroupDetails()
}

// notifyWebGroupChange notifies the web interface about group management changes
func (m *Manager) notifyWebGroupChange(eventType, groupName string) {
	// 检查EventBus是否可用
	if m.eventBus == nil {
		slog.Debug("[组管理] EventBus未设置，跳过组状态变化通知")
		return
	}

	// 获取组详细信息
	groupDetails := m.GetGroupDetails()

	// 构建事件数据
	data := map[string]interface{}{
		"event":     eventType,
		"group":     groupName,
		"timestamp": time.Now().Format("2006-01-02 15:04:05"),
		"details":   groupDetails,
	}

	// 使用EventBus发布组状态变化事件
	m.eventBus.Publish(events.Event{
		Type:      events.EventGroupStatusChanged,
		Source:    "endpoint_manager",
		Timestamp: time.Now(),
		Priority:  events.PriorityHigh,
		Data:      data,
	})

	slog.Debug(fmt.Sprintf("📢 [组管理] 发布组状态变化事件: %s (组: %s)", eventType, groupName))
}

// notifyGroupHealthStats 通知组健康统计变化（v4.0: 已弃用，前端不再监听）
func (m *Manager) notifyGroupHealthStats(groupName string) {
	// v4.0: 前端不再监听此事件，整个函数已禁用
	if m.eventBus == nil {
		return
	}

	// 处理空组名，默认为"Default"
	if groupName == "" {
		groupName = "Default"
	}

	// v4.0: 前端不再监听组健康统计事件，注释掉以减少无用开销
	// 获取组详细信息
	// groupDetails := m.groupManager.GetGroupDetails()
	// if groups, ok := groupDetails["groups"].([]map[string]interface{}); ok {
	// 	// 查找目标组的健康统计
	// 	for _, group := range groups {
	// 		if groupNameStr, exists := group["name"]; exists && groupNameStr == groupName {
	// 			// 发布组健康统计变化事件
	// 			m.eventBus.Publish(events.Event{
	// 				Type:     events.EventGroupHealthStatsChanged,
	// 				Source:   "endpoint_manager",
	// 				Priority: events.PriorityHigh,
	// 				Data: map[string]interface{}{
	// 					"group":               groupName,
	// 					"healthy_endpoints":   group["healthy_endpoints"],
	// 					"unhealthy_endpoints": group["unhealthy_endpoints"],
	// 					"total_endpoints":     group["total_endpoints"],
	// 					"is_active":           group["is_active"],
	// 					"status":              group["status"],
	// 					"change_type":         "health_stats_changed",
	// 					"timestamp":           time.Now().Format("2006-01-02 15:04:05"),
	// 				},
	// 			})
	//
	// 			slog.Debug(fmt.Sprintf("📊 [组健康统计] 成功发布组健康统计变化事件: %s (健康: %v/%v)",
	// 				groupName, group["healthy_endpoints"], group["total_endpoints"]))
	// 			return
	// 		}
	// 	}
	// }

	// v4.0: 组健康统计不再使用，未找到也不警告
}

// healthCheckLoop runs the health check routine
func (m *Manager) healthCheckLoop() {
	defer m.wg.Done()

	// 获取当前检查间隔
	getCheckInterval := func() time.Duration {
		interval := m.config.Health.CheckInterval
		if interval <= 0 {
			interval = 30 * time.Second // 默认30秒检查一次
		}
		return interval
	}

	currentInterval := getCheckInterval()
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	// Initial health check
	m.performHealthChecks()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.performHealthChecks()

			// 检查配置是否变化，动态调整间隔
			newInterval := getCheckInterval()
			if newInterval != currentInterval {
				slog.Info("🔄 [健康检查] 间隔已更新", "old", currentInterval, "new", newInterval)
				currentInterval = newInterval
				ticker.Reset(currentInterval)
			}
		}
	}
}

// performHealthChecks performs health checks on all endpoints
func (m *Manager) performHealthChecks() {
	// v5.0+: 使用快照机制，不阻塞动态增删操作
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()

	// v5.0: SQLite 存储模式下始终检查所有端点（不管 enabled 状态）
	// v4.0: YAML 配置模式下根据 auto/manual 模式决定
	var endpointsToCheck []*Endpoint

	// 判断是否为 SQLite 存储模式
	isSQLiteMode := m.config.EndpointsStorage.Type == "sqlite"

	if isSQLiteMode {
		// v5.0 SQLite 模式：检查所有端点（包括 enabled=false 的）
		endpointsToCheck = snapshot

		if len(endpointsToCheck) == 0 {
			slog.Debug("🩺 [健康检查] 没有配置的端点，跳过健康检查")
			return
		}

		slog.Debug(fmt.Sprintf("🩺 [健康检查] SQLite 模式：检查所有 %d 个端点（包括未激活）",
			len(endpointsToCheck)))
	} else if m.config.Group.AutoSwitchBetweenGroups {
		// v4.0 Auto mode: only check active group endpoints
		endpointsToCheck = m.groupManager.FilterEndpointsByActiveGroups(snapshot)

		if len(endpointsToCheck) == 0 {
			slog.Debug("🩺 [健康检查] 自动模式下没有活跃组中的端点，跳过健康检查")
			return
		}

		slog.Debug(fmt.Sprintf("🩺 [健康检查] 自动模式：开始检查 %d 个活跃组端点 (总共 %d 个端点)",
			len(endpointsToCheck), len(snapshot)))
	} else {
		// v4.0 Manual mode: check all endpoints to determine their health status
		endpointsToCheck = snapshot

		if len(endpointsToCheck) == 0 {
			slog.Debug("🩺 [健康检查] 没有配置的端点，跳过健康检查")
			return
		}

		slog.Debug(fmt.Sprintf("🩺 [健康检查] 手动模式：检查所有 %d 个端点的健康状态",
			len(endpointsToCheck)))
	}
	
	var wg sync.WaitGroup
	
	// Check the determined endpoints based on mode
	for _, endpoint := range endpointsToCheck {
		wg.Add(1)
		go func(ep *Endpoint) {
			defer wg.Done()
			m.checkEndpointHealth(ep)
		}(endpoint)
	}
	
	wg.Wait()
	
	// Count healthy endpoints after checks
	healthyCount := 0
	for _, ep := range endpointsToCheck {
		if ep.IsHealthy() {
			healthyCount++
		}
	}
	
	if m.config.Group.AutoSwitchBetweenGroups {
		slog.Debug(fmt.Sprintf("🩺 [健康检查] 完成检查 - 活跃组健康: %d/%d", healthyCount, len(endpointsToCheck)))
	} else {
		slog.Debug(fmt.Sprintf("🩺 [健康检查] 完成检查 - 总体健康: %d/%d", healthyCount, len(endpointsToCheck)))
	}

	// v5.0+ Wails 桌面应用：定时健康检查完成后触发回调推送事件
	if m.onHealthCheckComplete != nil {
		go m.onHealthCheckComplete()
	}
}

// checkEndpointHealth checks the health of a single endpoint
func (m *Manager) checkEndpointHealth(endpoint *Endpoint) {
	start := time.Now()
	
	healthURL := endpoint.Config.URL + m.config.Health.HealthPath
	req, err := http.NewRequestWithContext(m.ctx, "GET", healthURL, nil)
	if err != nil {
		m.updateEndpointStatus(endpoint, false, 0)
		return
	}

	// Add authorization header with dynamically resolved token
	token := m.GetTokenForEndpoint(endpoint)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := m.client.Do(req)
	responseTime := time.Since(start)
	
	if err != nil {
		// Network or connection error
		slog.Warn(fmt.Sprintf("❌ [健康检查] 端点网络错误: %s - 错误: %s, 响应时间: %dms", 
			endpoint.Config.Name, err.Error(), responseTime.Milliseconds()))
		m.updateEndpointStatus(endpoint, false, responseTime)
		return
	}
	
	resp.Body.Close()
	
	// Only consider 2xx as healthy for API endpoints
	// 2xx: Success responses only
	// All other status codes (including 4xx, 5xx) are considered unhealthy
	healthy := (resp.StatusCode >= 200 && resp.StatusCode < 300)
	
	// Log health check results
	if healthy {
		slog.Debug(fmt.Sprintf("✅ [健康检查] 端点正常: %s - 状态码: %d, 响应时间: %dms",
			endpoint.Config.Name,
			resp.StatusCode,
			responseTime.Milliseconds()))
	} else {
		slog.Warn(fmt.Sprintf("⚠️ [健康检查] 端点异常: %s - 状态码: %d, 响应时间: %dms",
			endpoint.Config.Name,
			resp.StatusCode,
			responseTime.Milliseconds()))
	}
	
	m.updateEndpointStatus(endpoint, healthy, responseTime)
}

// updateEndpointStatus updates the health status of an endpoint
func (m *Manager) updateEndpointStatus(endpoint *Endpoint, healthy bool, responseTime time.Duration) {
	endpoint.mutex.Lock()
	defer endpoint.mutex.Unlock()

	endpoint.Status.LastCheck = time.Now()
	endpoint.Status.ResponseTime = responseTime
	endpoint.Status.NeverChecked = false // 标记为已检测

	if healthy {
		// Endpoint is healthy
		wasUnhealthy := !endpoint.Status.Healthy
		endpoint.Status.Healthy = true
		endpoint.Status.ConsecutiveFails = 0

		// Log recovery if endpoint was previously unhealthy
		if wasUnhealthy {
			slog.Info(fmt.Sprintf("✅ [健康检查] 端点恢复正常: %s - 响应时间: %dms",
				endpoint.Config.Name, responseTime.Milliseconds()))
		}
	} else {
		// Endpoint failed health check
		endpoint.Status.ConsecutiveFails++
		wasHealthy := endpoint.Status.Healthy

		// Mark as unhealthy immediately on any failure
		endpoint.Status.Healthy = false

		// Log the failure
		if wasHealthy {
			slog.Warn(fmt.Sprintf("❌ [健康检查] 端点标记为不可用: %s - 连续失败: %d次, 响应时间: %dms",
				endpoint.Config.Name, endpoint.Status.ConsecutiveFails, responseTime.Milliseconds()))
		} else {
			slog.Debug(fmt.Sprintf("❌ [健康检查] 端点仍然不可用: %s - 连续失败: %d次, 响应时间: %dms",
				endpoint.Config.Name, endpoint.Status.ConsecutiveFails, responseTime.Milliseconds()))
		}
	}

	// 通知Web界面端点状态变化
	go m.notifyWebInterface(endpoint)

	// v4.0: 组健康统计已禁用，前端不再需要
	// 通知组健康统计变化
	// go m.notifyGroupHealthStats(endpoint.Config.Group)
}

// IsHealthy returns the health status of an endpoint
func (e *Endpoint) IsHealthy() bool {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.Status.Healthy
}

// GetResponseTime returns the last response time of an endpoint
func (e *Endpoint) GetResponseTime() time.Duration {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.Status.ResponseTime
}

// GetStatus returns a copy of the endpoint status
func (e *Endpoint) GetStatus() EndpointStatus {
	e.mutex.RLock()
	defer e.mutex.RUnlock()
	return e.Status
}

// GetEndpoints returns all endpoints for Web interface
func (m *Manager) GetEndpoints() []*Endpoint {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	result := make([]*Endpoint, len(m.endpoints))
	copy(result, m.endpoints)
	return result
}

// GetEndpointStatus returns the status of an endpoint by name
func (m *Manager) GetEndpointStatus(name string) EndpointStatus {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()

	for _, ep := range m.endpoints {
		if ep.Config.Name == name {
			ep.mutex.RLock()
			status := ep.Status
			ep.mutex.RUnlock()
			return status
		}
	}
	return EndpointStatus{}
}

// UpdateEndpointPriority updates the priority of an endpoint by name
func (m *Manager) UpdateEndpointPriority(name string, newPriority int) error {
	if newPriority < 1 {
		return fmt.Errorf("优先级必须大于等于1")
	}

	m.endpointsMu.RLock()
	// Find the endpoint
	var targetEndpoint *Endpoint
	for _, ep := range m.endpoints {
		if ep.Config.Name == name {
			targetEndpoint = ep
			break
		}
	}
	m.endpointsMu.RUnlock()

	if targetEndpoint == nil {
		return fmt.Errorf("端点 '%s' 未找到", name)
	}

	// Update the priority
	targetEndpoint.Config.Priority = newPriority

	// Update the config as well
	for i, epConfig := range m.config.Endpoints {
		if epConfig.Name == name {
			m.config.Endpoints[i].Priority = newPriority
			break
		}
	}

	slog.Info(fmt.Sprintf("🔄 端点优先级已更新: %s -> %d", name, newPriority))
	
	return nil
}

// ManualHealthCheck performs a manual health check on a specific endpoint by name
func (m *Manager) ManualHealthCheck(endpointName string) error {
	var targetEndpoint *Endpoint

	// v5.0+: 使用读锁查找端点
	m.endpointsMu.RLock()
	for _, endpoint := range m.endpoints {
		if endpoint.Config.Name == endpointName {
			targetEndpoint = endpoint
			break
		}
	}
	m.endpointsMu.RUnlock()

	if targetEndpoint == nil {
		return fmt.Errorf("端点 '%s' 未找到", endpointName)
	}

	// Perform health check on the endpoint
	slog.Info(fmt.Sprintf("🔍 [手动检查] 开始检查端点: %s", endpointName))
	m.checkEndpointHealth(targetEndpoint)

	// Get status and log completion with response time
	status := targetEndpoint.Status
	healthStatus := "健康"
	if !status.Healthy {
		if status.NeverChecked {
			healthStatus = "未检测"
		} else {
			healthStatus = "不健康"
		}
	}

	slog.Info(fmt.Sprintf("🔍 [手动检查] 检查完成: %s - 状态: %s, 响应时间: %s",
		endpointName, healthStatus, utils.FormatResponseTime(status.ResponseTime)))

	return nil
}

// BatchHealthCheckAll 批量检测所有端点的健康状态
// 并发执行所有端点的健康检查，提高效率
// 使用信号量限制并发数量，避免资源耗尽
func (m *Manager) BatchHealthCheckAll() (int, int, error) {
	slog.Info("🔍 [批量健康检测] 开始检测所有端点")

	// v5.0+: 使用快照机制获取端点列表
	m.endpointsMu.RLock()
	endpoints := make([]*Endpoint, len(m.endpoints))
	copy(endpoints, m.endpoints)
	m.endpointsMu.RUnlock()

	if len(endpoints) == 0 {
		return 0, 0, fmt.Errorf("没有配置任何端点")
	}

	slog.Info(fmt.Sprintf("   共有 %d 个端点需要检测", len(endpoints)))

	// 使用信号量限制并发数量（最多 20 个并发）
	const maxConcurrentChecks = 20
	semaphore := make(chan struct{}, maxConcurrentChecks)

	// 使用 WaitGroup 并发检测所有端点
	var wg sync.WaitGroup
	var healthyCount, unhealthyCount int
	var countMu sync.Mutex

	for _, endpoint := range endpoints {
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(ep *Endpoint) {
			defer wg.Done()
			defer func() { <-semaphore }() // 释放信号量

			// 执行健康检测（复用现有方法）
			m.checkEndpointHealth(ep)

			// 获取检测结果（需要加锁读取）
			ep.mutex.RLock()
			healthy := ep.Status.Healthy
			responseTime := ep.Status.ResponseTime
			ep.mutex.RUnlock()

			// 统计检测结果
			countMu.Lock()
			if healthy {
				healthyCount++
			} else {
				unhealthyCount++
			}
			countMu.Unlock()

			// 记录检测结果
			healthStatus := "❌ 不健康"
			if healthy {
				healthStatus = "✅ 健康"
			}
			slog.Debug(fmt.Sprintf("   检测端点: %s - 状态: %s, 响应时间: %s",
				ep.Config.Name,
				healthStatus,
				utils.FormatResponseTime(responseTime),
			))
		}(endpoint)
	}

	// 等待所有检测完成
	wg.Wait()

	slog.Info(fmt.Sprintf("✅ [批量健康检测] 完成，共检测 %d 个端点 (健康: %d, 不健康: %d)",
		len(endpoints), healthyCount, unhealthyCount))

	return healthyCount, unhealthyCount, nil
}

// ==================== 多 API Key 切换功能 ====================

// GetKeyManager 返回 Key 管理器
func (m *Manager) GetKeyManager() *KeyManager {
	return m.keyManager
}

// SwitchEndpointToken 切换端点的 Token
func (m *Manager) SwitchEndpointToken(endpointName string, index int) error {
	// 验证端点存在
	ep := m.GetEndpointByNameAny(endpointName)
	if ep == nil {
		return fmt.Errorf("端点 '%s' 未找到", endpointName)
	}

	// 验证该端点支持多 Token
	if len(ep.Config.Tokens) == 0 {
		return fmt.Errorf("端点 '%s' 未配置多 Token", endpointName)
	}

	err := m.keyManager.SwitchToken(endpointName, index)
	if err != nil {
		return err
	}

	// 获取切换后的 Token 名称用于日志
	tokenName := ""
	if index >= 0 && index < len(ep.Config.Tokens) {
		tokenName = ep.Config.Tokens[index].Name
		if tokenName == "" {
			tokenName = fmt.Sprintf("Token %d", index+1)
		}
	}

	slog.Info(fmt.Sprintf("🔑 [Key切换] 端点 %s 的 Token 已切换到: %s (索引: %d)", endpointName, tokenName, index))

	// 发布事件通知
	if m.eventBus != nil {
		m.eventBus.Publish(events.Event{
			Type:     "endpoint_key_changed",
			Source:   "key_manager",
			Priority: events.PriorityHigh,
			Data: map[string]interface{}{
				"endpoint":   endpointName,
				"key_type":   "token",
				"new_index":  index,
				"key_name":   tokenName,
				"timestamp":  time.Now().Format("2006-01-02 15:04:05"),
			},
		})
	}

	return nil
}

// SwitchEndpointApiKey 切换端点的 API Key
func (m *Manager) SwitchEndpointApiKey(endpointName string, index int) error {
	ep := m.GetEndpointByNameAny(endpointName)
	if ep == nil {
		return fmt.Errorf("端点 '%s' 未找到", endpointName)
	}

	if len(ep.Config.ApiKeys) == 0 {
		return fmt.Errorf("端点 '%s' 未配置多 API Key", endpointName)
	}

	err := m.keyManager.SwitchApiKey(endpointName, index)
	if err != nil {
		return err
	}

	// 获取切换后的 API Key 名称用于日志
	keyName := ""
	if index >= 0 && index < len(ep.Config.ApiKeys) {
		keyName = ep.Config.ApiKeys[index].Name
		if keyName == "" {
			keyName = fmt.Sprintf("API Key %d", index+1)
		}
	}

	slog.Info(fmt.Sprintf("🔑 [Key切换] 端点 %s 的 API Key 已切换到: %s (索引: %d)", endpointName, keyName, index))

	if m.eventBus != nil {
		m.eventBus.Publish(events.Event{
			Type:     "endpoint_key_changed",
			Source:   "key_manager",
			Priority: events.PriorityHigh,
			Data: map[string]interface{}{
				"endpoint":   endpointName,
				"key_type":   "api_key",
				"new_index":  index,
				"key_name":   keyName,
				"timestamp":  time.Now().Format("2006-01-02 15:04:05"),
			},
		})
	}

	return nil
}

// GetEndpointKeysInfo 获取端点的 Key 信息（用于 API，Key 值脱敏）
func (m *Manager) GetEndpointKeysInfo(endpointName string) map[string]interface{} {
	ep := m.GetEndpointByNameAny(endpointName)
	if ep == nil {
		return nil
	}

	state := m.keyManager.GetEndpointKeyState(endpointName)

	// 构建 Token 列表（脱敏）
	tokens := make([]map[string]interface{}, 0)
	for i, t := range ep.Config.Tokens {
		tokens = append(tokens, map[string]interface{}{
			"index":     i,
			"name":      t.Name,
			"masked":    maskKey(t.Value),
			"is_active": state != nil && state.ActiveTokenIndex == i,
		})
	}
	// 单 Token 情况
	if len(tokens) == 0 && ep.Config.Token != "" {
		tokens = append(tokens, map[string]interface{}{
			"index":     0,
			"name":      "default",
			"masked":    maskKey(ep.Config.Token),
			"is_active": true,
		})
	}

	// 构建 API Key 列表（脱敏）
	apiKeys := make([]map[string]interface{}, 0)
	for i, k := range ep.Config.ApiKeys {
		apiKeys = append(apiKeys, map[string]interface{}{
			"index":     i,
			"name":      k.Name,
			"masked":    maskKey(k.Value),
			"is_active": state != nil && state.ActiveApiKeyIndex == i,
		})
	}
	if len(apiKeys) == 0 && ep.Config.ApiKey != "" {
		apiKeys = append(apiKeys, map[string]interface{}{
			"index":     0,
			"name":      "default",
			"masked":    maskKey(ep.Config.ApiKey),
			"is_active": true,
		})
	}

	result := map[string]interface{}{
		"endpoint":           endpointName,
		"tokens":             tokens,
		"api_keys":           apiKeys,
		"supports_switching": len(ep.Config.Tokens) > 1 || len(ep.Config.ApiKeys) > 1,
	}

	if state != nil && !state.LastSwitchTime.IsZero() {
		result["last_switch_time"] = state.LastSwitchTime.Format("2006-01-02 15:04:05")
	}

	return result
}

// maskKey 脱敏 Key 值，只显示前4位和后4位
func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// ==================== v5.0+ 动态端点管理功能 ====================

// SyncEndpoints 从数据库同步端点（v5.0 Desktop 专用）
// 用于启动时从数据库加载端点，替换现有端点列表
func (m *Manager) SyncEndpoints(configs []config.EndpointConfig) {
	// 创建新端点列表
	endpoints := make([]*Endpoint, len(configs))
	for i, cfg := range configs {
		endpoints[i] = &Endpoint{
			Config: cfg,
			Status: EndpointStatus{
				Healthy:      false,
				LastCheck:    time.Now(),
				NeverChecked: true,
			},
		}

		// 初始化 Key 管理状态
		tokenCount := len(cfg.Tokens)
		if tokenCount == 0 && cfg.Token != "" {
			tokenCount = 1
		}
		apiKeyCount := len(cfg.ApiKeys)
		if apiKeyCount == 0 && cfg.ApiKey != "" {
			apiKeyCount = 1
		}
		m.keyManager.InitEndpoint(cfg.Name, tokenCount, apiKeyCount)
	}

	// 使用写锁替换端点列表
	m.endpointsMu.Lock()
	m.endpoints = endpoints
	m.endpointsMu.Unlock()

	// 更新 GroupManager（创建组）
	m.groupManager.UpdateGroups(endpoints)

	slog.Info(fmt.Sprintf("🔄 [端点同步] 已同步 %d 个端点到管理器", len(configs)))
}

// AddEndpoint 动态添加端点（v5.0+ 新增）
// 线程安全地将新端点添加到管理器中
func (m *Manager) AddEndpoint(cfg config.EndpointConfig) error {
	// 验证端点名称唯一性
	m.endpointsMu.RLock()
	for _, ep := range m.endpoints {
		if ep.Config.Name == cfg.Name {
			m.endpointsMu.RUnlock()
			return fmt.Errorf("端点 '%s' 已存在", cfg.Name)
		}
	}
	m.endpointsMu.RUnlock()

	// 创建新端点
	endpoint := &Endpoint{
		Config: cfg,
		Status: EndpointStatus{
			Healthy:      false, // 悲观初始化，等待健康检查
			LastCheck:    time.Now(),
			NeverChecked: true,
		},
	}

	// 初始化 Key 管理状态
	tokenCount := len(cfg.Tokens)
	if tokenCount == 0 && cfg.Token != "" {
		tokenCount = 1
	}
	apiKeyCount := len(cfg.ApiKeys)
	if apiKeyCount == 0 && cfg.ApiKey != "" {
		apiKeyCount = 1
	}
	m.keyManager.InitEndpoint(cfg.Name, tokenCount, apiKeyCount)

	// 使用写锁添加端点
	m.endpointsMu.Lock()
	m.endpoints = append(m.endpoints, endpoint)
	m.endpointsMu.Unlock()

	// 更新 GroupManager
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()
	m.groupManager.UpdateGroups(snapshot)

	// 立即触发健康检查
	go m.checkEndpointHealth(endpoint)

	// 发布事件通知
	if m.eventBus != nil {
		m.eventBus.Publish(events.Event{
			Type:     "endpoint_added",
			Source:   "endpoint_manager",
			Priority: events.PriorityHigh,
			Data: map[string]interface{}{
				"name":      cfg.Name,
				"url":       cfg.URL,
				"priority":  cfg.Priority,
				"timestamp": time.Now().Format("2006-01-02 15:04:05"),
			},
		})
	}

	slog.Info(fmt.Sprintf("➕ [端点管理] 新增端点: %s (%s)", cfg.Name, cfg.URL))
	return nil
}

// RemoveEndpoint 动态移除端点（v5.0+ 新增）
// 线程安全地从管理器中移除端点
func (m *Manager) RemoveEndpoint(name string) error {
	m.endpointsMu.Lock()
	defer m.endpointsMu.Unlock()

	// 查找并移除端点
	index := -1
	for i, ep := range m.endpoints {
		if ep.Config.Name == name {
			index = i
			break
		}
	}

	if index == -1 {
		return fmt.Errorf("端点 '%s' 未找到", name)
	}

	// 移除端点（保持切片顺序）
	removedEndpoint := m.endpoints[index]
	m.endpoints = append(m.endpoints[:index], m.endpoints[index+1:]...)

	// 清理 KeyManager 状态
	m.keyManager.RemoveEndpoint(name)

	// 更新 GroupManager（在锁内创建快照）
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)

	// 在锁外更新 GroupManager
	go func() {
		m.groupManager.UpdateGroups(snapshot)
	}()

	// 发布事件通知
	if m.eventBus != nil {
		m.eventBus.Publish(events.Event{
			Type:     "endpoint_removed",
			Source:   "endpoint_manager",
			Priority: events.PriorityHigh,
			Data: map[string]interface{}{
				"name":      name,
				"url":       removedEndpoint.Config.URL,
				"timestamp": time.Now().Format("2006-01-02 15:04:05"),
			},
		})
	}

	slog.Info(fmt.Sprintf("➖ [端点管理] 移除端点: %s", name))
	return nil
}

// UpdateEndpointConfig 更新端点配置（v5.0+ 新增）
// 更新现有端点的配置（不包括名称）
func (m *Manager) UpdateEndpointConfig(name string, cfg config.EndpointConfig) error {
	m.endpointsMu.RLock()
	var targetEndpoint *Endpoint
	for _, ep := range m.endpoints {
		if ep.Config.Name == name {
			targetEndpoint = ep
			break
		}
	}
	m.endpointsMu.RUnlock()

	if targetEndpoint == nil {
		return fmt.Errorf("端点 '%s' 未找到", name)
	}

	// 保留原名称
	cfg.Name = name

	// 更新配置
	targetEndpoint.mutex.Lock()
	targetEndpoint.Config = cfg
	targetEndpoint.mutex.Unlock()

	// 更新 Key 管理状态
	tokenCount := len(cfg.Tokens)
	if tokenCount == 0 && cfg.Token != "" {
		tokenCount = 1
	}
	apiKeyCount := len(cfg.ApiKeys)
	if apiKeyCount == 0 && cfg.ApiKey != "" {
		apiKeyCount = 1
	}
	m.keyManager.UpdateEndpointKeyCount(name, tokenCount, apiKeyCount)

	// 更新 GroupManager
	m.endpointsMu.RLock()
	snapshot := make([]*Endpoint, len(m.endpoints))
	copy(snapshot, m.endpoints)
	m.endpointsMu.RUnlock()
	m.groupManager.UpdateGroups(snapshot)

	// 立即触发健康检查
	go m.checkEndpointHealth(targetEndpoint)

	// 发布事件通知
	if m.eventBus != nil {
		m.eventBus.Publish(events.Event{
			Type:     "endpoint_updated",
			Source:   "endpoint_manager",
			Priority: events.PriorityHigh,
			Data: map[string]interface{}{
				"name":      name,
				"url":       cfg.URL,
				"priority":  cfg.Priority,
				"timestamp": time.Now().Format("2006-01-02 15:04:05"),
			},
		})
	}

	slog.Info(fmt.Sprintf("✏️ [端点管理] 更新端点配置: %s", name))
	return nil
}

// GetEndpointCount 返回当前端点数量（v5.0+ 新增）
func (m *Manager) GetEndpointCount() int {
	m.endpointsMu.RLock()
	defer m.endpointsMu.RUnlock()
	return len(m.endpoints)
}