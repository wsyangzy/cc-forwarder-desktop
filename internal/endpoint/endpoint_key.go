package endpoint

import "cc-forwarder/config"

const endpointKeySeparator = "::"

// EndpointKey returns a stable identifier for an endpoint in runtime.
// - SQLite/channel 模式：使用 channel::name，避免不同渠道同名导致内部状态冲突。
// - YAML/旧模式：channel 为空时回退为 name（保持向后兼容）。
func EndpointKey(channel, name string) string {
	if channel == "" {
		return name
	}
	if name == "" {
		return channel
	}
	return channel + endpointKeySeparator + name
}

func endpointKeyFromConfig(cfg config.EndpointConfig) string {
	return EndpointKey(cfg.Channel, cfg.Name)
}

