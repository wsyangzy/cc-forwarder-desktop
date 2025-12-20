// app_api_endpoint.go - 端点管理 API (Wails Bindings)
// 包含端点状态、优先级设置、健康检查、Key 管理等功能

package main

import (
	"fmt"
	"time"
)

// ============================================================
// 端点管理 API
// ============================================================

// EndpointInfo 端点信息
type EndpointInfo struct {
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	Channel         string  `json:"channel"` // v5.0: 渠道标签
	Group           string  `json:"group"`
	Priority        int     `json:"priority"`
	GroupPriority   int     `json:"group_priority"`
	GroupIsActive   bool    `json:"group_is_active"`
	Healthy         bool    `json:"healthy"`
	LastCheck       string  `json:"last_check"`
	ResponseTimeMs  float64 `json:"response_time_ms"`
	ConsecutiveFail int     `json:"consecutive_fail"`
}

// GetEndpoints 获取所有端点状态
func (a *App) GetEndpoints() []EndpointInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.endpointManager == nil {
		return []EndpointInfo{}
	}

	endpoints := a.endpointManager.GetAllEndpoints()
	gm := a.endpointManager.GetGroupManager()
	result := make([]EndpointInfo, 0, len(endpoints))

	// 预先构建组状态映射
	groupActiveMap := make(map[string]bool)
	groupPriorityMap := make(map[string]int)
	if gm != nil {
		for _, g := range gm.GetAllGroups() {
			groupActiveMap[g.Name] = g.IsActive
			groupPriorityMap[g.Name] = g.Priority
		}
	}

	for _, ep := range endpoints {
		// v6.0: 路由组 = 渠道(channel)，未配置 channel 则回退为端点名（兼容旧逻辑）
		routeGroup := ep.Config.Channel
		if routeGroup == "" {
			routeGroup = ep.Config.Name
		}

		info := EndpointInfo{
			Name:            ep.Config.Name,
			URL:             ep.Config.URL,
			Channel:         ep.Config.Channel, // v5.0: 渠道标签
			Group:           routeGroup,
			Priority:        ep.Config.Priority,
			Healthy:         ep.Status.Healthy,
			ConsecutiveFail: ep.Status.ConsecutiveFails,
			ResponseTimeMs:  float64(ep.Status.ResponseTime.Milliseconds()),
		}

		// 获取组是否激活
		info.GroupIsActive = groupActiveMap[routeGroup]
		info.GroupPriority = groupPriorityMap[routeGroup]

		if !ep.Status.LastCheck.IsZero() {
			info.LastCheck = ep.Status.LastCheck.Format(time.RFC3339)
		}

		result = append(result, info)
	}

	return result
}

// SetEndpointPriority 设置端点优先级
func (a *App) SetEndpointPriority(name string, priority int) error {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.endpointManager == nil {
		return fmt.Errorf("端点管理器未初始化")
	}

	return a.endpointManager.UpdateEndpointPriority(name, priority)
}

// TriggerHealthCheck 手动触发健康检查
func (a *App) TriggerHealthCheck(name string) error {
	a.mu.RLock()
	manager := a.endpointManager
	a.mu.RUnlock()

	if manager == nil {
		return fmt.Errorf("端点管理器未初始化")
	}

	err := manager.ManualHealthCheck(name)
	if err != nil {
		return err
	}

	// 健康检查完成后，推送端点状态更新到前端
	go a.emitEndpointUpdate()

	return nil
}

// BatchHealthCheckResult 批量健康检查结果
type BatchHealthCheckResult struct {
	Success      bool   `json:"success"`
	Message      string `json:"message"`
	Total        int    `json:"total"`
	HealthyCount int    `json:"healthy_count"`
	UnhealthyCount int  `json:"unhealthy_count"`
}

// BatchHealthCheckAll 批量检查所有端点的健康状态
func (a *App) BatchHealthCheckAll() BatchHealthCheckResult {
	a.mu.RLock()
	manager := a.endpointManager
	a.mu.RUnlock()

	if manager == nil {
		return BatchHealthCheckResult{
			Success: false,
			Message: "端点管理器未初始化",
		}
	}

	// 调用 endpointManager 的批量健康检查
	healthyCount, unhealthyCount, err := manager.BatchHealthCheckAll()
	if err != nil {
		return BatchHealthCheckResult{
			Success: false,
			Message: err.Error(),
		}
	}

	// 推送端点状态更新到前端
	go a.emitEndpointUpdate()

	return BatchHealthCheckResult{
		Success:        true,
		Message:        "批量健康检测完成",
		Total:          healthyCount + unhealthyCount,
		HealthyCount:   healthyCount,
		UnhealthyCount: unhealthyCount,
	}
}

// ============================================================
// Key 管理 API
// ============================================================

// KeyInfo Key 信息结构
type KeyInfo struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`      // Key 名称
	Value    string `json:"value"`     // 脱敏后的值 (masked)
	IsActive bool   `json:"is_active"` // 是否为当前使用的 Key
}

// EndpointKeysInfo 端点 Key 概览
type EndpointKeysInfo struct {
	Endpoint           string    `json:"endpoint"`
	Tokens             []KeyInfo `json:"tokens"`
	ApiKeys            []KeyInfo `json:"api_keys"`
	CurrentTokenIndex  int       `json:"current_token_index"`
	CurrentApiKeyIndex int       `json:"current_api_key_index"`
}

// KeysOverviewResult Keys 概览结果
type KeysOverviewResult struct {
	Endpoints []EndpointKeysInfo `json:"endpoints"`
	Total     int                `json:"total"`
	Timestamp string             `json:"timestamp"`
}

// GetKeysOverview 获取所有端点的 Key 概览
func (a *App) GetKeysOverview() KeysOverviewResult {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := KeysOverviewResult{
		Endpoints: make([]EndpointKeysInfo, 0),
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
	}

	if a.endpointManager == nil {
		return result
	}

	endpoints := a.endpointManager.GetAllEndpoints()
	for _, ep := range endpoints {
		keysInfo := a.endpointManager.GetEndpointKeysInfo(ep.Config.Name)
		if keysInfo == nil {
			continue
		}

		// 转换为前端期望的格式
		info := EndpointKeysInfo{
			Endpoint: ep.Config.Name,
			Tokens:   make([]KeyInfo, 0),
			ApiKeys:  make([]KeyInfo, 0),
		}

		// 解析 keysInfo map - 注意类型是 []map[string]interface{} 不是 []interface{}
		if tokens, ok := keysInfo["tokens"].([]map[string]interface{}); ok {
			for i, tokenMap := range tokens {
				keyInfo := KeyInfo{
					Index:    i,
					IsActive: false,
				}
				if name, ok := tokenMap["name"].(string); ok {
					keyInfo.Name = name
				}
				if v, ok := tokenMap["masked"].(string); ok {
					keyInfo.Value = v
				}
				if active, ok := tokenMap["is_active"].(bool); ok {
					keyInfo.IsActive = active
					if active {
						info.CurrentTokenIndex = i
					}
				}
				info.Tokens = append(info.Tokens, keyInfo)
			}
		}

		if apiKeys, ok := keysInfo["api_keys"].([]map[string]interface{}); ok {
			for i, keyMap := range apiKeys {
				keyInfo := KeyInfo{
					Index:    i,
					IsActive: false,
				}
				if name, ok := keyMap["name"].(string); ok {
					keyInfo.Name = name
				}
				if v, ok := keyMap["masked"].(string); ok {
					keyInfo.Value = v
				}
				if active, ok := keyMap["is_active"].(bool); ok {
					keyInfo.IsActive = active
					if active {
						info.CurrentApiKeyIndex = i
					}
				}
				info.ApiKeys = append(info.ApiKeys, keyInfo)
			}
		}

		result.Endpoints = append(result.Endpoints, info)
	}

	result.Total = len(result.Endpoints)
	return result
}

// SwitchKeyResult 切换 Key 结果
type SwitchKeyResult struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Endpoint  string `json:"endpoint"`
	KeyType   string `json:"key_type"`
	NewIndex  int    `json:"new_index"`
	Timestamp string `json:"timestamp"`
}

// SwitchKey 切换端点的 Token 或 API Key
// keyType: "token" 或 "api_key"
func (a *App) SwitchKey(endpointName, keyType string, index int) (SwitchKeyResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := SwitchKeyResult{
		Endpoint:  endpointName,
		KeyType:   keyType,
		NewIndex:  index,
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
	}

	if a.endpointManager == nil {
		return result, fmt.Errorf("端点管理器未初始化")
	}

	var err error
	switch keyType {
	case "token":
		err = a.endpointManager.SwitchEndpointToken(endpointName, index)
		if err == nil {
			result.Success = true
			result.Message = "Token 切换成功"
			if a.logger != nil {
				a.logger.Info("🔑 Token已通过桌面应用切换", "endpoint", endpointName, "index", index)
			}
		}
	case "api_key":
		err = a.endpointManager.SwitchEndpointApiKey(endpointName, index)
		if err == nil {
			result.Success = true
			result.Message = "API Key 切换成功"
			if a.logger != nil {
				a.logger.Info("🔑 API Key已通过桌面应用切换", "endpoint", endpointName, "index", index)
			}
		}
	default:
		return result, fmt.Errorf("无效的 Key 类型: %s (应为 token 或 api_key)", keyType)
	}

	if err != nil {
		return result, err
	}

	return result, nil
}
