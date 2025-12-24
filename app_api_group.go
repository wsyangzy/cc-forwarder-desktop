// app_api_group.go - 组管理 API (Wails Bindings)
// 包含组状态查询、激活、暂停、恢复等功能
// 2025-12-06 10:49:09 v5.0: 激活端点时同步到数据库

package main

import (
	"context"
	"fmt"
	"time"
)

// ============================================================
// 组管理 API
// ============================================================

// GroupInfo 组信息
type GroupInfo struct {
	Name             string `json:"name"`
	Channel          string `json:"channel"` // v5.0: 渠道名称（从端点配置获取）
	Active           bool   `json:"active"`
	Paused           bool   `json:"paused"`
	Priority         int    `json:"priority"`
	EndpointCount    int    `json:"endpoint_count"`
	InCooldown       bool   `json:"in_cooldown"`
	CooldownRemainMs int64  `json:"cooldown_remain_ms"`
}

// GetGroups 获取所有组状态
func (a *App) GetGroups() []GroupInfo {
	a.mu.RLock()
	manager := a.endpointManager
	a.mu.RUnlock()

	if manager == nil {
		return []GroupInfo{}
	}

	gm := manager.GetGroupManager()
	if gm == nil {
		return []GroupInfo{}
	}

	groups := gm.GetAllGroups()
	result := make([]GroupInfo, 0, len(groups))

	for _, g := range groups {
		// 从第一个端点获取渠道名称
		channel := ""
		if len(g.Endpoints) > 0 {
			channel = g.Endpoints[0].Config.Channel
		}
		if channel == "" {
			channel = g.Name
		}

		info := GroupInfo{
			Name:          g.Name,
			Channel:       channel,
			Active:        g.IsActive,
			Paused:        g.ManuallyPaused,
			Priority:      g.Priority,
			EndpointCount: len(g.Endpoints),
			InCooldown:    gm.IsGroupInCooldown(g.Name),
		}

		// 获取冷却剩余时间
		remaining := gm.GetGroupCooldownRemaining(g.Name)
		if remaining > 0 {
			info.CooldownRemainMs = remaining.Milliseconds()
		}

		result = append(result, info)
	}

	return result
}

// ActivateGroup 激活指定组
// v6.0: 组名 = 渠道(channel)（SQLite 模式）；未配置 channel 时回退为端点名（YAML 模式兼容）
func (a *App) ActivateGroup(name string) error {
	a.mu.RLock()
	manager := a.endpointManager
	endpointService := a.endpointService
	logger := a.logger
	a.mu.RUnlock()

	if manager == nil {
		return fmt.Errorf("端点管理器未初始化")
	}

	// v5.0+: 如果有 endpointService，同步到数据库
	if endpointService != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// SQLite：激活渠道（互斥）
		if err := endpointService.ActivateChannel(ctx, name); err != nil {
			if logger != nil {
				logger.Warn("激活渠道失败", "channel", name, "error", err)
			}
		} else {
			if logger != nil {
				logger.Info("✅ 渠道已同步到数据库", "channel", name, "enabled", true)
			}
			return nil
		}
	}

	// 内存中激活组（兼容 YAML 模式）
	return manager.ManualActivateGroup(name)
}

// PauseGroup 暂停指定组
func (a *App) PauseGroup(name string) error {
	a.mu.RLock()
	manager := a.endpointManager
	channelService := a.channelService
	a.mu.RUnlock()

	if manager == nil {
		return fmt.Errorf("端点管理器未初始化")
	}

	// v6.2+: 暂停/恢复为“无限期直到手动恢复”，且在 SQLite 模式下持久化到 channels.failover_enabled
	if channelService != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := channelService.SetFailoverEnabled(ctx, name, false); err != nil {
			return err
		}
		a.syncChannelFailoverEnabledToEndpointManager(ctx)
		a.emitEndpointUpdate()
		return nil
	}

	return manager.ManualPauseGroup(name, 0)
}

// ResumeGroup 恢复指定组
func (a *App) ResumeGroup(name string) error {
	a.mu.RLock()
	manager := a.endpointManager
	channelService := a.channelService
	a.mu.RUnlock()

	if manager == nil {
		return fmt.Errorf("端点管理器未初始化")
	}

	if channelService != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := channelService.SetFailoverEnabled(ctx, name, true); err != nil {
			return err
		}
		a.syncChannelFailoverEnabledToEndpointManager(ctx)
		a.emitEndpointUpdate()
		return nil
	}

	return manager.ManualResumeGroup(name)
}
