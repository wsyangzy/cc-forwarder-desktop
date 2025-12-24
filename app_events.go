// app_events.go - Wails 事件发射
// 将 Go 后端状态变化通知到前端

package main

import (
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// 事件名称常量
const (
	EventSystemStatus   = "system:status"
	EventEndpointUpdate = "endpoint:update"
	EventGroupUpdate    = "group:update"
	EventUsageUpdate    = "usage:update"
	EventConfigReloaded = "config:reloaded"
	EventError          = "error"
	EventNotification   = "notification"
)

// emitSystemStatus 发送系统状态更新到前端
func (a *App) emitSystemStatus() {
	if a.ctx == nil {
		return
	}

	status := a.GetSystemStatus()
	runtime.EventsEmit(a.ctx, EventSystemStatus, status)
}

// emitEndpointUpdate 发送端点状态更新到前端
func (a *App) emitEndpointUpdate() {
	if a.ctx == nil {
		return
	}

	endpoints := a.GetEndpoints()
	// 包装为前端期望的格式 { endpoints: [...] }
	data := map[string]interface{}{
		"endpoints": endpoints,
	}

	if a.logger != nil {
		a.logger.Debug("📡 [Wails Event] 推送端点更新", "count", len(endpoints))
	}

	runtime.EventsEmit(a.ctx, EventEndpointUpdate, data)
}

// emitGroupUpdate 发送组状态更新到前端
func (a *App) emitGroupUpdate() {
	if a.ctx == nil {
		return
	}

	groups := a.GetGroups()
	runtime.EventsEmit(a.ctx, EventGroupUpdate, groups)
}

// emitUsageUpdate 发送使用统计更新到前端
func (a *App) emitUsageUpdate() {
	if a.ctx == nil {
		return
	}

	summary, _ := a.GetUsageSummary("", "")
	runtime.EventsEmit(a.ctx, EventUsageUpdate, summary)
}

// emitNotification 发送通知到前端
func (a *App) emitNotification(level, title, message string) {
	if a.ctx == nil {
		return
	}

	runtime.EventsEmit(a.ctx, EventNotification, map[string]string{
		"level":   level, // "info", "warning", "error", "success"
		"title":   title,
		"message": message,
	})
}
