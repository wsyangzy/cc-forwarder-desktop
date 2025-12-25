package endpoint

import (
	"fmt"
	"sync"
	"time"
)

// KeyManager 管理所有端点的 API Key 状态
// 支持每个端点独立管理多个 Token 和 API Key，通过索引切换当前使用的 Key
type KeyManager struct {
	states map[string]*EndpointKeyState // endpointKey -> state
	mu     sync.RWMutex
}

// EndpointKeyState 端点的 Key 状态
type EndpointKeyState struct {
	EndpointName      string    // 端点标识（channel::name 或 name，历史字段名保留以兼容测试/外部调用）
	ActiveTokenIndex  int       // 当前激活的 Token 索引
	ActiveApiKeyIndex int       // 当前激活的 API Key 索引
	TokenCount        int       // Token 总数
	ApiKeyCount       int       // API Key 总数
	LastSwitchTime    time.Time // 最后切换时间
	mu                sync.RWMutex
}

// NewKeyManager 创建新的 Key 管理器
func NewKeyManager() *KeyManager {
	return &KeyManager{
		states: make(map[string]*EndpointKeyState),
	}
}

// InitEndpoint 初始化端点的 Key 状态
func (km *KeyManager) InitEndpoint(endpointKey string, tokenCount, apiKeyCount int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	km.states[endpointKey] = &EndpointKeyState{
		EndpointName:      endpointKey,
		ActiveTokenIndex:  0, // 默认使用第一个
		ActiveApiKeyIndex: 0,
		TokenCount:        tokenCount,
		ApiKeyCount:       apiKeyCount,
	}
}

// GetActiveTokenIndex 获取当前激活的 Token 索引
func (km *KeyManager) GetActiveTokenIndex(endpointKey string) int {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if state, exists := km.states[endpointKey]; exists {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.ActiveTokenIndex
	}
	return 0
}

// GetActiveApiKeyIndex 获取当前激活的 API Key 索引
func (km *KeyManager) GetActiveApiKeyIndex(endpointKey string) int {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if state, exists := km.states[endpointKey]; exists {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.ActiveApiKeyIndex
	}
	return 0
}

// SwitchToken 切换端点的 Token
func (km *KeyManager) SwitchToken(endpointKey string, index int) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	state, exists := km.states[endpointKey]
	if !exists {
		return fmt.Errorf("端点 '%s' 未找到", endpointKey)
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if index < 0 || index >= state.TokenCount {
		return fmt.Errorf("Token 索引 %d 超出范围 [0, %d)", index, state.TokenCount)
	}

	state.ActiveTokenIndex = index
	state.LastSwitchTime = time.Now()
	return nil
}

// SwitchApiKey 切换端点的 API Key
func (km *KeyManager) SwitchApiKey(endpointKey string, index int) error {
	km.mu.Lock()
	defer km.mu.Unlock()

	state, exists := km.states[endpointKey]
	if !exists {
		return fmt.Errorf("端点 '%s' 未找到", endpointKey)
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if index < 0 || index >= state.ApiKeyCount {
		return fmt.Errorf("API Key 索引 %d 超出范围 [0, %d)", index, state.ApiKeyCount)
	}

	state.ActiveApiKeyIndex = index
	state.LastSwitchTime = time.Now()
	return nil
}

// GetEndpointKeyState 获取端点的完整 Key 状态（返回副本，线程安全）
func (km *KeyManager) GetEndpointKeyState(endpointKey string) *EndpointKeyState {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if state, exists := km.states[endpointKey]; exists {
		state.mu.RLock()
		defer state.mu.RUnlock()
		// 返回副本
		return &EndpointKeyState{
			EndpointName:      state.EndpointName,
			ActiveTokenIndex:  state.ActiveTokenIndex,
			ActiveApiKeyIndex: state.ActiveApiKeyIndex,
			TokenCount:        state.TokenCount,
			ApiKeyCount:       state.ApiKeyCount,
			LastSwitchTime:    state.LastSwitchTime,
		}
	}
	return nil
}

// GetAllStates 获取所有端点的 Key 状态（用于 API）
func (km *KeyManager) GetAllStates() map[string]*EndpointKeyState {
	km.mu.RLock()
	defer km.mu.RUnlock()

	result := make(map[string]*EndpointKeyState)
	for name, state := range km.states {
		state.mu.RLock()
		result[name] = &EndpointKeyState{
			EndpointName:      state.EndpointName,
			ActiveTokenIndex:  state.ActiveTokenIndex,
			ActiveApiKeyIndex: state.ActiveApiKeyIndex,
			TokenCount:        state.TokenCount,
			ApiKeyCount:       state.ApiKeyCount,
			LastSwitchTime:    state.LastSwitchTime,
		}
		state.mu.RUnlock()
	}
	return result
}

// HasMultipleTokens 检查端点是否配置了多个 Token
func (km *KeyManager) HasMultipleTokens(endpointKey string) bool {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if state, exists := km.states[endpointKey]; exists {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.TokenCount > 1
	}
	return false
}

// HasMultipleApiKeys 检查端点是否配置了多个 API Key
func (km *KeyManager) HasMultipleApiKeys(endpointKey string) bool {
	km.mu.RLock()
	defer km.mu.RUnlock()

	if state, exists := km.states[endpointKey]; exists {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.ApiKeyCount > 1
	}
	return false
}

// UpdateEndpointKeyCount 更新端点的 Key 数量（用于配置热更新）
func (km *KeyManager) UpdateEndpointKeyCount(endpointKey string, tokenCount, apiKeyCount int) {
	km.mu.Lock()
	defer km.mu.Unlock()

	if state, exists := km.states[endpointKey]; exists {
		state.mu.Lock()
		defer state.mu.Unlock()

		state.TokenCount = tokenCount
		state.ApiKeyCount = apiKeyCount

		// 如果当前索引超出新范围，重置为 0
		if state.ActiveTokenIndex >= tokenCount {
			state.ActiveTokenIndex = 0
		}
		if state.ActiveApiKeyIndex >= apiKeyCount {
			state.ActiveApiKeyIndex = 0
		}
	} else {
		// 端点不存在，创建新状态
		km.states[endpointKey] = &EndpointKeyState{
			EndpointName:      endpointKey,
			ActiveTokenIndex:  0,
			ActiveApiKeyIndex: 0,
			TokenCount:        tokenCount,
			ApiKeyCount:       apiKeyCount,
		}
	}
}

// RemoveEndpoint 移除端点的 Key 状态（用于配置热更新）
func (km *KeyManager) RemoveEndpoint(endpointKey string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	delete(km.states, endpointKey)
}

// RenameEndpointKey 在端点标识发生变化时迁移 Key 状态（例如：端点改名或移动渠道）。
// 注意：调用方需保证 newKey 不与现有端点冲突。
func (km *KeyManager) RenameEndpointKey(oldKey, newKey string) {
	if oldKey == "" || newKey == "" || oldKey == newKey {
		return
	}

	km.mu.Lock()
	defer km.mu.Unlock()

	state, ok := km.states[oldKey]
	if !ok {
		return
	}
	delete(km.states, oldKey)
	state.mu.Lock()
	state.EndpointName = newKey
	state.mu.Unlock()
	km.states[newKey] = state
}
