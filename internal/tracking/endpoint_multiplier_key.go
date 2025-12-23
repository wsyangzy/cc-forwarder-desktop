package tracking

const endpointMultiplierKeySeparator = "::"

// endpointMultiplierKey builds the lookup key for endpoint multipliers.
// v6.2+: 允许不同渠道端点同名，为避免倍率冲突，倍率缓存以 "channel::endpointName" 为 key。
// 兼容：若 channel 为空则回退到 groupName，再回退到 endpointName。
func endpointMultiplierKey(channel, groupName, endpointName string) string {
	if endpointName == "" {
		return ""
	}
	if channel == "" {
		channel = groupName
	}
	if channel == "" {
		return endpointName
	}
	return channel + endpointMultiplierKeySeparator + endpointName
}

// EndpointMultiplierKey is the exported helper for building the multiplier key from channel + endpointName.
func EndpointMultiplierKey(channel, endpointName string) string {
	return endpointMultiplierKey(channel, "", endpointName)
}
