package endpoint

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"cc-forwarder/config"
)

// GroupInfo represents information about an endpoint group
type GroupInfo struct {
	Name          string
	Priority      int
	IsActive      bool
	CooldownUntil time.Time
	Endpoints     []*Endpoint
	// Manual control states
	ManuallyPaused       bool
	ManualActivationTime time.Time
	// Forced activation states
	ForcedActivation     bool      // 标记是否为强制激活（无健康端点时激活）
	ForcedActivationTime time.Time // 强制激活时间
}

// GroupManager manages endpoint groups and their cooldown states
type GroupManager struct {
	groups                 map[string]*GroupInfo
	config                 *config.Config
	mutex                  sync.RWMutex
	cooldownDuration       time.Duration
	channelPriorities      map[string]int
	channelFailoverEnabled map[string]bool
	// Group change notification subscribers
	groupChangeSubscribers []chan string
	subscriberMutex        sync.RWMutex
}

// NewGroupManager creates a new group manager
// v4.0: Support both old Group config and new Failover config
func NewGroupManager(cfg *config.Config) *GroupManager {
	// v4.0: 优先使用 Failover 配置，如果没有则使用 Group 配置（向后兼容）
	cooldownDuration := cfg.Group.Cooldown
	if cfg.Failover.DefaultCooldown > 0 {
		cooldownDuration = cfg.Failover.DefaultCooldown
	}
	if cooldownDuration == 0 {
		cooldownDuration = 10 * time.Minute
	}

	return &GroupManager{
		groups:                 make(map[string]*GroupInfo),
		config:                 cfg,
		cooldownDuration:       cooldownDuration,
		channelPriorities:      make(map[string]int),
		channelFailoverEnabled: make(map[string]bool),
		groupChangeSubscribers: make([]chan string, 0),
	}
}

// UpdateConfig updates the group manager configuration
// v4.0: Support both old Group config and new Failover config
func (gm *GroupManager) UpdateConfig(cfg *config.Config) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	gm.config = cfg

	// v4.0: 优先使用 Failover 配置，如果没有则使用 Group 配置（向后兼容）
	if cfg.Failover.DefaultCooldown > 0 {
		gm.cooldownDuration = cfg.Failover.DefaultCooldown
	} else {
		gm.cooldownDuration = cfg.Group.Cooldown
	}
	if gm.cooldownDuration == 0 {
		gm.cooldownDuration = 10 * time.Minute
	}
}

// UpdateChannelPriorities 更新“渠道(channel)”的优先级映射。
// v6.1.0: 渠道优先级仅用于“渠道间”故障转移顺序；渠道内端点故障转移仍由端点 priority 决定。
func (gm *GroupManager) UpdateChannelPriorities(priorities map[string]int) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	next := make(map[string]int, len(priorities))
	for name, p := range priorities {
		if name == "" {
			continue
		}
		if p <= 0 {
			p = 1
		}
		next[name] = p
	}
	gm.channelPriorities = next

	// 尽最大努力即时同步到已存在的组（不依赖 UpdateGroups 重建）
	for name, group := range gm.groups {
		if group == nil {
			continue
		}
		if p, ok := gm.channelPriorities[name]; ok {
			group.Priority = p
		}
	}
}

// UpdateChannelFailoverEnabled 更新“渠道(channel)”是否参与渠道间故障转移的开关映射。
// 该开关用于前端“暂停/恢复”按钮的持久化状态，并影响：
// - 跨渠道故障转移的候选筛选（跳过暂停渠道）
// - 定时健康检查/延迟检测的覆盖范围
func (gm *GroupManager) UpdateChannelFailoverEnabled(enabled map[string]bool) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	next := make(map[string]bool, len(enabled))
	for name, v := range enabled {
		if name == "" {
			continue
		}
		next[name] = v
	}
	gm.channelFailoverEnabled = next

	// 同步到已存在的组（尽最大努力即时生效）
	for name, group := range gm.groups {
		if group == nil {
			continue
		}
		if v, ok := gm.channelFailoverEnabled[name]; ok && !v {
			group.ManuallyPaused = true
		} else {
			group.ManuallyPaused = false
		}
	}
}

// IsChannelFailoverEnabled 查询“渠道是否参与渠道间故障转移”。
// 默认值为 true（未配置时视为参与）。
func (gm *GroupManager) IsChannelFailoverEnabled(channel string) bool {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()
	if channel == "" {
		return true
	}
	v, ok := gm.channelFailoverEnabled[channel]
	if !ok {
		return true
	}
	return v
}

// UpdateGroups rebuilds group information from endpoints
// v4.0: Automatically creates one group per endpoint
func (gm *GroupManager) UpdateGroups(endpoints []*Endpoint) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	// v5.0: SQLite 模式下需要保留 IsActive 状态
	isSQLiteMode := gm.config.EndpointsStorage.Type == "sqlite"

	// Clear existing groups but preserve cooldown states (and IsActive for SQLite mode)
	oldGroups := make(map[string]*GroupInfo)
	for name, group := range gm.groups {
		// v5.0: SQLite 模式下保留 IsActive 状态
		if isSQLiteMode || (!group.CooldownUntil.IsZero() && time.Now().Before(group.CooldownUntil)) {
			oldGroups[name] = &GroupInfo{
				Name:          group.Name,
				Priority:      group.Priority,
				IsActive:      group.IsActive, // v5.0: 保留激活状态
				CooldownUntil: group.CooldownUntil,
				Endpoints:     nil, // Will be updated
			}
		}
	}

	// Rebuild groups from current endpoints
	// v6.0: 以“渠道(channel)”作为路由与故障转移单位；未配置 channel 则回退为端点名（兼容旧逻辑）
	newGroups := make(map[string]*GroupInfo)

	for _, ep := range endpoints {
		groupName := ChannelKey(ep)

		group, exists := newGroups[groupName]
		if !exists {
			// Check if this group was in cooldown or had active state
			var cooldownUntil time.Time
			var wasActive bool
			if oldGroup, existed := oldGroups[groupName]; existed {
				cooldownUntil = oldGroup.CooldownUntil
				wasActive = oldGroup.IsActive // v5.0: 恢复之前的激活状态
			}

			group = &GroupInfo{
				Name:          groupName,
				Endpoints:     make([]*Endpoint, 0, 2),
				IsActive:      wasActive, // v5.0: SQLite 模式下保留之前的激活状态
				CooldownUntil: cooldownUntil,
				Priority:      ep.Config.Priority,
			}
			newGroups[groupName] = group
		}

		group.Endpoints = append(group.Endpoints, ep)

		// 组优先级：取组内最小 endpoint priority（越小越优先）
		if ep.Config.Priority < group.Priority {
			group.Priority = ep.Config.Priority
		}
	}

	// v6.1.0: 渠道优先级优先于端点优先级（仅用于渠道间选择顺序）
	for name, group := range newGroups {
		if group == nil {
			continue
		}
		if p, ok := gm.channelPriorities[name]; ok && p > 0 {
			group.Priority = p
			continue
		}
		if group.Priority <= 0 {
			group.Priority = 1
		}
	}

	// 渠道级暂停（持久化）：由 channels.failover_enabled 控制
	for name, group := range newGroups {
		if group == nil {
			continue
		}
		if v, ok := gm.channelFailoverEnabled[name]; ok && !v {
			group.ManuallyPaused = true
		} else {
			group.ManuallyPaused = false
		}
	}

	// 端点级兜底：当且仅当组内所有端点都 failover_enabled=false 时，该组无法参与跨渠道故障转移。
	// 为保持旧行为/测试预期，这种情况下也视为“暂停”（不会落库，只是运行时派生）。
	for _, group := range newGroups {
		if group == nil {
			continue
		}
		allDisabled := true
		for _, ep := range group.Endpoints {
			failoverEnabled := true
			if ep != nil && ep.Config.FailoverEnabled != nil {
				failoverEnabled = *ep.Config.FailoverEnabled
			}
			if failoverEnabled {
				allDisabled = false
				break
			}
		}
		if allDisabled {
			group.ManuallyPaused = true
		}
	}

	gm.groups = newGroups

	// Update active status based on cooldown timers
	gm.updateActiveGroups()
}

// updateActiveGroups updates which groups are currently active
func (gm *GroupManager) updateActiveGroups() {
	// v5.0: SQLite 模式下，禁用自动激活逻辑（由 enabled 字段控制）
	// 但仍需处理冷却超时清理
	isSQLiteMode := gm.config.EndpointsStorage.Type == "sqlite"
	// v6.0: Failover.Enabled 仅控制“渠道间”故障转移/自动切换行为
	autoSwitchEnabled := gm.config.Failover.Enabled

	now := time.Now()
	var newlyActivatedGroup string

	// Track previous active state to detect changes
	previousActiveGroups := make(map[string]bool)
	for _, group := range gm.groups {
		previousActiveGroups[group.Name] = group.IsActive
	}

	// First, check cooldown timers and clear expired cooldowns
	for _, group := range gm.groups {
		if !group.CooldownUntil.IsZero() && now.After(group.CooldownUntil) {
			// Cooldown expired, clear it but don't auto-activate in manual mode
			group.CooldownUntil = time.Time{}
			slog.Info(fmt.Sprintf("🔄 [组管理] 组冷却结束: %s (优先级: %d) - %s",
				group.Name, group.Priority,
				map[bool]string{true: "自动激活", false: "等待手动激活"}[autoSwitchEnabled]))
		} else if !group.CooldownUntil.IsZero() && now.Before(group.CooldownUntil) {
			// Still in cooldown
			group.IsActive = false
		}
	}

	// v5.0: SQLite 模式下跳过自动激活逻辑（手动控制），仅处理冷却状态即可
	if isSQLiteMode {
		return
	}

	// Determine which groups should be active based on priority
	// Only auto-activate next group if auto switching is enabled
	if autoSwitchEnabled {
		// Auto mode: automatically activate highest priority available group
		// Get all groups sorted by priority
		sortedGroups := gm.getSortedGroups()

		// Find the highest priority group that's not in cooldown and not manually paused
		activeGroupFound := false
		for _, group := range sortedGroups {
			isAvailable := group.CooldownUntil.IsZero() && !group.ManuallyPaused
			if isAvailable {
				if !activeGroupFound {
					wasActive := group.IsActive
					group.IsActive = true
					activeGroupFound = true
					// Check if this group became newly active
					if !wasActive && group.IsActive {
						newlyActivatedGroup = group.Name
					}
				} else {
					group.IsActive = false // Only one group can be active at a time
				}
			} else {
				group.IsActive = false
			}
		}
	} else {
		// Manual mode: Only activate priority 1 group at startup if no groups are active
		// Don't auto-switch between groups during runtime
		currentActiveCount := 0
		for _, group := range gm.groups {
			if group.IsActive {
				currentActiveCount++
			}
		}

		// Handle cooldown states first
		for _, group := range gm.groups {
			if !group.CooldownUntil.IsZero() && now.Before(group.CooldownUntil) {
				// Still in cooldown, keep inactive
				group.IsActive = false
			}
		}

		// If no groups are active, determine if this is startup or runtime failure
		if currentActiveCount == 0 {
			// Check if this is truly startup (no groups have ever failed) or runtime failure
			isActualStartup := true
			for _, group := range gm.groups {
				if !group.CooldownUntil.IsZero() || group.ManuallyPaused {
					isActualStartup = false
					break
				}
			}

			// Determine activation strategy based on startup vs runtime context
			var shouldAutoActivate bool
			if isActualStartup {
				// 启动时：激活最高优先级可用组（更符合用户直觉）
				shouldAutoActivate = true
				slog.Debug("🚀 [组管理] 检测到系统启动 - 尝试激活最高优先级可用组")
			} else {
				// This is runtime failure - respect manual mode + suspend settings
				// v6.0: Failover.Enabled 仅控制“渠道间”故障转移/自动切换行为
				autoSwitchEnabled := gm.config.Failover.Enabled
				if !autoSwitchEnabled && gm.config.RequestSuspend.Enabled {
					shouldAutoActivate = false
					slog.Debug("⏸️ [组管理] 运行时故障且启用挂起 - 不激活其他组，等待挂起处理")
				} else {
					// Manual mode without suspend, or auto mode - allow activation
					shouldAutoActivate = true
					slog.Debug("🔄 [组管理] 运行时故障但未启用挂起 - 尝试激活可用组")
				}
			}

			if shouldAutoActivate {
				sortedGroups := gm.getSortedGroups()
				for _, group := range sortedGroups {
					// 关键修复：检查组是否被手动暂停（包括因失败而暂停的组）
					if group.CooldownUntil.IsZero() && !group.ManuallyPaused {
						// Check if this group has healthy endpoints
						hasHealthyEndpoints := false
						for _, ep := range group.Endpoints {
							if ep.IsHealthy() {
								hasHealthyEndpoints = true
								break
							}
						}
						if hasHealthyEndpoints {
							wasActive := group.IsActive
							group.IsActive = true
							// v6.0: Failover.Enabled 仅控制“渠道间”故障转移/自动切换行为
							autoSwitchEnabled := gm.config.Failover.Enabled
							if isActualStartup {
								if autoSwitchEnabled {
									slog.Info(fmt.Sprintf("🚀 [自动模式] 启动时激活最高优先级可用组: %s (有健康端点)", group.Name))
								} else {
									slog.Info(fmt.Sprintf("🚀 [手动模式] 启动时激活最高优先级可用组: %s (有健康端点) - 后续故障将启用挂起", group.Name))
								}
							} else {
								slog.Info(fmt.Sprintf("🔄 [运行时] 激活可用组: %s (优先级: %d, 有健康端点)", group.Name, group.Priority))
							}
							// Check if this group became newly active
							if !wasActive && group.IsActive {
								newlyActivatedGroup = group.Name
							}
							break // Only activate one group
						}
					} else if group.ManuallyPaused {
						// 记录被暂停的组，说明为什么没有激活
						slog.Debug(fmt.Sprintf("⏸️ [手动模式] 跳过已暂停组: %s (优先级: %d) - 等待手动恢复", group.Name, group.Priority))
					}
				}
			}
		}
	}

	// Notify subscribers if a group was newly activated
	if newlyActivatedGroup != "" {
		// Check if this is truly a state change (not just the same group remaining active)
		if !previousActiveGroups[newlyActivatedGroup] {
			slog.Debug(fmt.Sprintf("📡 [组通知] 检测到组状态变化: %s 变为活跃", newlyActivatedGroup))
			gm.notifyGroupChange(newlyActivatedGroup)
		}
	}
}

// getSortedGroups returns groups sorted by priority (lower number = higher priority)
func (gm *GroupManager) getSortedGroups() []*GroupInfo {
	groups := make([]*GroupInfo, 0, len(gm.groups))
	for _, group := range gm.groups {
		groups = append(groups, group)
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Priority != groups[j].Priority {
			return groups[i].Priority < groups[j].Priority
		}
		return groups[i].Name < groups[j].Name
	})

	return groups
}

// GetActiveGroups returns currently active groups
func (gm *GroupManager) GetActiveGroups() []*GroupInfo {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	gm.updateActiveGroups()

	var active []*GroupInfo
	for _, group := range gm.groups {
		if group.IsActive {
			active = append(active, group)
		}
	}

	// Sort by priority
	sort.Slice(active, func(i, j int) bool {
		if active[i].Priority != active[j].Priority {
			return active[i].Priority < active[j].Priority
		}
		return active[i].Name < active[j].Name
	})

	return active
}

// GetAllGroups returns all groups
func (gm *GroupManager) GetAllGroups() []*GroupInfo {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	gm.updateActiveGroups()

	groups := make([]*GroupInfo, 0, len(gm.groups))
	for _, group := range gm.groups {
		groups = append(groups, group)
	}

	// Sort by priority
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Priority != groups[j].Priority {
			return groups[i].Priority < groups[j].Priority
		}
		return groups[i].Name < groups[j].Name
	})

	return groups
}

// SetGroupCooldown sets a group into cooldown mode (only in auto mode)
func (gm *GroupManager) SetGroupCooldown(groupName string) {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	if group, exists := gm.groups[groupName]; exists {
		// In manual mode, mark group as manually paused to prevent re-activation
		// v6.0: Failover.Enabled 仅控制“渠道间”故障转移/自动切换行为
		autoSwitchEnabled := gm.config.Failover.Enabled
		if !autoSwitchEnabled {
			group.IsActive = false
			group.ManuallyPaused = true // 👈 关键修复：防止组被自动重新激活
			slog.Warn(fmt.Sprintf("⚠️ [手动模式] 组 %s 失败已停用并标记为暂停状态，需要手动切换到其他组", groupName))
			slog.Info(fmt.Sprintf("🚫 [组状态] 组 %s 已设置 ManuallyPaused=true，不会被自动重新激活", groupName))
			return
		}

		// Auto mode: use cooldown mechanism
		now := time.Now()
		group.CooldownUntil = now.Add(gm.cooldownDuration)
		group.IsActive = false

		slog.Warn(fmt.Sprintf("❄️ [自动模式] 组进入冷却状态: %s (冷却时长: %v, 恢复时间: %s)",
			groupName, gm.cooldownDuration, group.CooldownUntil.Format("15:04:05")))

		// Update active groups after cooldown change
		gm.updateActiveGroups()

		// Log and notify about next active group
		for _, g := range gm.getSortedGroups() {
			if g.IsActive {
				slog.Info(fmt.Sprintf("🔄 [自动模式] 切换到下一优先级组: %s (优先级: %d)",
					g.Name, g.Priority))
				// Notify subscribers about the group switch
				gm.notifyGroupChange(g.Name)
				break
			}
		}
	}
}

// IsGroupInCooldown checks if a group is currently in cooldown
func (gm *GroupManager) IsGroupInCooldown(groupName string) bool {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	if group, exists := gm.groups[groupName]; exists {
		return !group.CooldownUntil.IsZero() && time.Now().Before(group.CooldownUntil)
	}

	return false
}

// GetGroupCooldownRemaining returns remaining cooldown time for a group
func (gm *GroupManager) GetGroupCooldownRemaining(groupName string) time.Duration {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	if group, exists := gm.groups[groupName]; exists {
		if !group.CooldownUntil.IsZero() && time.Now().Before(group.CooldownUntil) {
			return group.CooldownUntil.Sub(time.Now())
		}
	}

	return 0
}

// ManualActivateGroup manually activates a specific group and deactivates others (compatibility function)
func (gm *GroupManager) ManualActivateGroup(groupName string) error {
	return gm.ManualActivateGroupWithForce(groupName, false)
}

// ManualActivateGroupWithForce manually activates a specific group and deactivates others
// force: 当为true时，即使组内没有健康端点也强制激活
func (gm *GroupManager) ManualActivateGroupWithForce(groupName string, force bool) error {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	targetGroup, exists := gm.groups[groupName]
	if !exists {
		return fmt.Errorf("组不存在: %s", groupName)
	}

	// 检查冷却状态（强制激活仍需检查冷却）
	if !targetGroup.CooldownUntil.IsZero() && time.Now().Before(targetGroup.CooldownUntil) {
		remaining := targetGroup.CooldownUntil.Sub(time.Now())
		return fmt.Errorf("组 %s 仍在冷却中，剩余时间: %v", groupName, remaining.Round(time.Second))
	}

	// v5.0: SQLite 模式下跳过健康检查（因为启动时健康检查还没开始）
	isSQLiteMode := gm.config.EndpointsStorage.Type == "sqlite"

	// 检查健康端点
	healthyCount := 0
	totalCount := len(targetGroup.Endpoints)
	for _, ep := range targetGroup.Endpoints {
		if ep.IsHealthy() {
			healthyCount++
		}
	}

	// v5.0: SQLite 模式下，允许激活没有健康端点的组（用户手动控制）
	if isSQLiteMode {
		// SQLite 模式：直接激活，不检查健康状态
		if healthyCount == 0 {
			slog.Info(fmt.Sprintf("🔄 [SQLite模式] 激活端点: %s (健康检查待执行)", groupName))
		} else {
			slog.Info(fmt.Sprintf("🔄 [SQLite模式] 激活端点: %s (健康端点: %d/%d)", groupName, healthyCount, totalCount))
		}
	} else {
		// YAML 模式：保持原有的健康检查逻辑
		// 核心逻辑：强制激活只能在完全没有健康端点时使用
		if healthyCount == 0 {
			// 没有健康端点的情况
			if !force {
				return fmt.Errorf("组 %s 中没有健康的端点，无法激活。如需强制激活请使用强制模式", groupName)
			}
			// 强制激活：只有在没有健康端点时才允许
			slog.Warn(fmt.Sprintf("⚠️ [强制激活] 用户强制激活无健康端点组: %s (健康端点: %d/%d, 操作时间: %s, 风险等级: HIGH)",
				groupName, healthyCount, totalCount, time.Now().Format("2006-01-02 15:04:05")))
			slog.Error(fmt.Sprintf("🚨 [安全警告] 强制激活可能导致请求失败! 组: %s, 建议尽快检查端点健康状态", groupName))

			// 标记强制激活
			targetGroup.ForcedActivation = true
			targetGroup.ForcedActivationTime = time.Now()
		} else {
			// 有健康端点的情况
			if force {
				// 拒绝在有健康端点时使用强制激活
				return fmt.Errorf("组 %s 有 %d 个健康端点，无需强制激活。请使用正常激活", groupName, healthyCount)
			}
			// 正常激活
			targetGroup.ForcedActivation = false
			targetGroup.ForcedActivationTime = time.Time{}

			slog.Info(fmt.Sprintf("🔄 [正常激活] 手动激活组: %s (健康端点: %d/%d)",
				groupName, healthyCount, totalCount))
		}
	}

	// 停用所有组
	for _, group := range gm.groups {
		group.IsActive = false
	}

	// 激活目标组
	targetGroup.IsActive = true
	targetGroup.ManualActivationTime = time.Now()
	targetGroup.CooldownUntil = time.Time{}

	// 通知订阅者
	gm.notifyGroupChange(groupName)

	return nil
}

// DeactivateGroup 停用指定组（用于故障转移时停用失败的端点）
// 注意：这只是简单地设置 IsActive=false，不设置 ManuallyPaused 标志
func (gm *GroupManager) DeactivateGroup(groupName string) error {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	targetGroup, exists := gm.groups[groupName]
	if !exists {
		return fmt.Errorf("组不存在: %s", groupName)
	}

	if targetGroup.IsActive {
		targetGroup.IsActive = false
		slog.Info(fmt.Sprintf("🔴 [组管理] 组已停用: %s", groupName))
	}

	return nil
}

// ManualPauseGroup manually pauses a group (prevents it from being auto-activated)
func (gm *GroupManager) ManualPauseGroup(groupName string, duration time.Duration) error {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	targetGroup, exists := gm.groups[groupName]
	if !exists {
		return fmt.Errorf("组不存在: %s", groupName)
	}

	// Pause the group
	targetGroup.ManuallyPaused = true
	gm.channelFailoverEnabled[groupName] = false

	// 非 SQLite 模式下，“暂停渠道”应立即让其退出活跃状态，避免继续被选中。
	// SQLite 模式下活跃状态由 enabled 字段/显式激活控制，这里不强制切换。
	isSQLiteMode := gm.config.EndpointsStorage.Type == "sqlite"
	if !isSQLiteMode && targetGroup.IsActive {
		targetGroup.IsActive = false
	}
	// 重新评估活跃组（非 SQLite 模式下可即时切换到其他可用渠道）
	gm.updateActiveGroups()

	if duration > 0 {
		// Set a timer to automatically unpause
		go func() {
			time.Sleep(duration)
			gm.mutex.Lock()
			defer gm.mutex.Unlock()
			if targetGroup.ManuallyPaused {
				targetGroup.ManuallyPaused = false
				gm.channelFailoverEnabled[groupName] = true
				// Store previous state to check for changes
				prevActiveGroups := make(map[string]bool)
				for _, g := range gm.groups {
					prevActiveGroups[g.Name] = g.IsActive
				}
				gm.updateActiveGroups()
				// Check if any group became newly active
				for _, g := range gm.groups {
					if g.IsActive && !prevActiveGroups[g.Name] {
						gm.notifyGroupChange(g.Name)
						break
					}
				}
				slog.Info(fmt.Sprintf("⏰ [自动恢复] 组 %s 暂停期已结束，重新可用", groupName))
			}
		}()
		slog.Info(fmt.Sprintf("⏸️ [手动暂停] 组 %s 已暂停 %v", groupName, duration))
	} else {
		slog.Info(fmt.Sprintf("⏸️ [手动暂停] 组 %s 已暂停，需要手动恢复", groupName))
	}

	return nil
}

// ManualResumeGroup manually resumes a paused group
func (gm *GroupManager) ManualResumeGroup(groupName string) error {
	gm.mutex.Lock()
	defer gm.mutex.Unlock()

	targetGroup, exists := gm.groups[groupName]
	if !exists {
		return fmt.Errorf("组不存在: %s", groupName)
	}

	if !targetGroup.ManuallyPaused {
		return fmt.Errorf("组 %s 未处于暂停状态", groupName)
	}

	targetGroup.ManuallyPaused = false
	gm.channelFailoverEnabled[groupName] = true

	// Store previous active groups to detect changes
	prevActiveGroups := make(map[string]bool)
	for _, g := range gm.groups {
		prevActiveGroups[g.Name] = g.IsActive
	}

	gm.updateActiveGroups() // Re-evaluate active groups

	// Check if any group became newly active
	for _, g := range gm.groups {
		if g.IsActive && !prevActiveGroups[g.Name] {
			gm.notifyGroupChange(g.Name)
			slog.Debug(fmt.Sprintf("📡 [组通知] 因恢复组 %s 而激活组 %s", groupName, g.Name))
			break
		}
	}

	slog.Info(fmt.Sprintf("▶️ [手动恢复] 组 %s 已恢复，重新参与自动选择", groupName))
	return nil
}

// GetGroupDetails returns detailed information about all groups
func (gm *GroupManager) GetGroupDetails() map[string]interface{} {
	gm.mutex.RLock()
	defer gm.mutex.RUnlock()

	gm.updateActiveGroups()

	result := make(map[string]interface{})
	groupsData := make([]map[string]interface{}, 0, len(gm.groups))

	for _, group := range gm.groups {
		healthyCount := 0
		unhealthyCount := 0
		totalEndpoints := len(group.Endpoints)

		for _, ep := range group.Endpoints {
			if ep.IsHealthy() {
				healthyCount++
			} else {
				unhealthyCount++
			}
		}

		var status string
		var statusColor string
		var cooldownRemaining time.Duration

		if group.IsActive {
			status = "活跃"
			statusColor = "success"
		} else if group.ManuallyPaused {
			status = "手动暂停"
			statusColor = "warning"
		} else if !group.CooldownUntil.IsZero() && time.Now().Before(group.CooldownUntil) {
			status = "冷却中"
			statusColor = "danger"
			cooldownRemaining = group.CooldownUntil.Sub(time.Now())
		} else if healthyCount == 0 {
			status = "无健康端点"
			statusColor = "danger"
		} else {
			status = "可用"
			statusColor = "secondary"
		}

		groupData := map[string]interface{}{
			"name":                   group.Name,
			"priority":               group.Priority,
			"is_active":              group.IsActive,
			"status":                 status,
			"status_color":           statusColor,
			"total_endpoints":        totalEndpoints,
			"healthy_endpoints":      healthyCount,
			"unhealthy_endpoints":    unhealthyCount,
			"manually_paused":        group.ManuallyPaused,
			"in_cooldown":            !group.CooldownUntil.IsZero() && time.Now().Before(group.CooldownUntil),
			"cooldown_remaining":     cooldownRemaining.Round(time.Second).String(),
			"can_activate":           healthyCount > 0 && !group.IsActive && (group.CooldownUntil.IsZero() || time.Now().After(group.CooldownUntil)),
			"can_pause":              !group.ManuallyPaused,
			"can_resume":             group.ManuallyPaused,
			"forced_activation":      group.ForcedActivation,
			"forced_activation_time": "",
			"activation_type":        "normal",
			"can_force_activate":     healthyCount == 0 && !group.IsActive && (group.CooldownUntil.IsZero() || time.Now().After(group.CooldownUntil)),
		}

		// 添加强制激活时间
		if !group.ForcedActivationTime.IsZero() {
			groupData["forced_activation_time"] = group.ForcedActivationTime.Format("2006-01-02 15:04:05")
		}

		// 设置激活类型
		if group.ForcedActivation {
			groupData["activation_type"] = "forced"
			// 计算健康状态描述
			if healthyCount == 0 {
				groupData["_computed_health_status"] = "强制激活(无健康端点)"
			} else {
				groupData["_computed_health_status"] = "强制激活(已恢复)"
			}
		}

		if !group.ManualActivationTime.IsZero() {
			groupData["last_manual_activation"] = group.ManualActivationTime.Format("2006-01-02 15:04:05")
		}

		groupsData = append(groupsData, groupData)
	}

	// Sort by priority
	sort.Slice(groupsData, func(i, j int) bool {
		return groupsData[i]["priority"].(int) < groupsData[j]["priority"].(int)
	})

	result["groups"] = groupsData
	result["total_groups"] = len(groupsData)
	result["active_groups"] = len(gm.GetActiveGroups())

	return result
}

// FilterEndpointsByActiveGroups filters endpoints to only include those in active groups
// v4.0: 一端点一组架构，组名 = 端点名
func (gm *GroupManager) FilterEndpointsByActiveGroups(endpoints []*Endpoint) []*Endpoint {
	activeGroups := gm.GetActiveGroups()
	if len(activeGroups) == 0 {
		return nil
	}

	// Create a map of active group names for quick lookup
	activeGroupNames := make(map[string]bool)
	for _, group := range activeGroups {
		activeGroupNames[group.Name] = true
	}

	// Filter endpoints
	// v6.0: 组名 = 渠道(channel)，未配置 channel 则回退为端点名
	var filtered []*Endpoint
	for _, ep := range endpoints {
		groupName := ChannelKey(ep)

		if activeGroupNames[groupName] {
			filtered = append(filtered, ep)
		}
	}

	return filtered
}

// SubscribeToGroupChanges subscribes to group change notifications
// Returns a channel that will receive the name of the newly activated group
func (gm *GroupManager) SubscribeToGroupChanges() <-chan string {
	gm.subscriberMutex.Lock()
	defer gm.subscriberMutex.Unlock()

	// Create a buffered channel to avoid blocking the sender
	ch := make(chan string, 10)
	gm.groupChangeSubscribers = append(gm.groupChangeSubscribers, ch)

	slog.Debug(fmt.Sprintf("📡 [组通知] 新增订阅者，当前订阅者数: %d", len(gm.groupChangeSubscribers)))

	return ch
}

// UnsubscribeFromGroupChanges removes a subscriber from group change notifications
func (gm *GroupManager) UnsubscribeFromGroupChanges(ch <-chan string) {
	gm.subscriberMutex.Lock()
	defer gm.subscriberMutex.Unlock()

	// Find and remove the channel from subscribers
	for i, subscriber := range gm.groupChangeSubscribers {
		if subscriber == ch {
			// Remove the channel from the slice
			gm.groupChangeSubscribers = append(gm.groupChangeSubscribers[:i], gm.groupChangeSubscribers[i+1:]...)
			// Close the channel to signal unsubscription
			close(subscriber)
			slog.Debug(fmt.Sprintf("📡 [组通知] 移除订阅者，当前订阅者数: %d", len(gm.groupChangeSubscribers)))
			return
		}
	}
}

// notifyGroupChange sends a non-blocking notification to all subscribers
// This method should be called with appropriate locks already held
func (gm *GroupManager) notifyGroupChange(activatedGroupName string) {
	gm.subscriberMutex.RLock()
	defer gm.subscriberMutex.RUnlock()

	if len(gm.groupChangeSubscribers) == 0 {
		return
	}

	slog.Debug(fmt.Sprintf("📡 [组通知] 广播组切换事件: %s (订阅者数: %d)",
		activatedGroupName, len(gm.groupChangeSubscribers)))

	// Send notification to all subscribers in a non-blocking manner
	for i, subscriber := range gm.groupChangeSubscribers {
		select {
		case subscriber <- activatedGroupName:
			// Successfully sent
		default:
			// Channel is full or closed, log warning
			slog.Warn(fmt.Sprintf("📡 [组通知] 订阅者 #%d 通道已满或已关闭，跳过通知", i))
		}
	}
}
