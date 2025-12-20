// Package service 提供业务逻辑层实现
// 渠道服务 - v6.1.0 新增
package service

import (
	"context"
	"fmt"

	"cc-forwarder/internal/store"
)

type ChannelService struct {
	store store.ChannelStore
}

func NewChannelService(store store.ChannelStore) *ChannelService {
	return &ChannelService{store: store}
}

func (s *ChannelService) CreateChannel(ctx context.Context, record *store.ChannelRecord) (*store.ChannelRecord, error) {
	if record == nil {
		return nil, fmt.Errorf("record 不能为空")
	}
	if record.Name == "" {
		return nil, fmt.Errorf("渠道名称不能为空")
	}
	if record.Priority <= 0 {
		record.Priority = 1
	}
	existing, err := s.store.Get(ctx, record.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("渠道 '%s' 已存在", record.Name)
	}
	return s.store.Create(ctx, record)
}

func (s *ChannelService) EnsureChannel(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("渠道名称不能为空")
	}
	existing, err := s.store.Get(ctx, name)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	_, err = s.store.Create(ctx, &store.ChannelRecord{Name: name})
	return err
}

func (s *ChannelService) ListChannels(ctx context.Context) ([]*store.ChannelRecord, error) {
	return s.store.List(ctx)
}

func (s *ChannelService) UpdateChannel(ctx context.Context, record *store.ChannelRecord) error {
	if record == nil {
		return fmt.Errorf("record 不能为空")
	}
	if record.Name == "" {
		return fmt.Errorf("渠道名称不能为空")
	}
	if record.Priority <= 0 {
		record.Priority = 1
	}
	existing, err := s.store.Get(ctx, record.Name)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("渠道不存在: %s", record.Name)
	}
	return s.store.Update(ctx, record)
}

// BackfillChannelsFromEndpoints 将历史 endpoints.channel 反写到 channels 表，确保“端点删光后渠道仍存在”。
// 说明：仅 best-effort，不因单个渠道写入失败而中断。
func (s *ChannelService) BackfillChannelsFromEndpoints(ctx context.Context, endpointStore store.EndpointStore) (int, error) {
	if s == nil || s.store == nil {
		return 0, nil
	}
	if endpointStore == nil {
		return 0, nil
	}

	endpoints, err := endpointStore.List(ctx)
	if err != nil {
		return 0, err
	}

	seen := make(map[string]struct{}, 16)
	added := 0
	for _, ep := range endpoints {
		if ep == nil || ep.Channel == "" {
			continue
		}
		if _, ok := seen[ep.Channel]; ok {
			continue
		}
		seen[ep.Channel] = struct{}{}
		if err := s.EnsureChannel(ctx, ep.Channel); err == nil {
			added++
		}
	}
	return added, nil
}
