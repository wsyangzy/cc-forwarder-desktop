// ============================================
// KPI 卡片网格组件
// 2025-12-06 v4.0: 一个端点 = 一个组
// ============================================

import {
  Zap,
  Clock,
  Server,
  Network,
  Activity
} from 'lucide-react';
import { KPICard } from '@components/ui';

const KPICardsGrid = ({ data }) => {
  const { status, endpoints, connections, groups } = data;

  // v4.0: 一个端点 = 一个组，显示当前活动端点
  // 优先从 groups 获取活跃状态，因为 group.is_active 才表示"正在使用"
  const activeGroup = groups.groups?.find(g => g.is_active);

  // 构建显示文本
  let activeEndpointText = '无活动端点';
  if (activeGroup) {
    // v4.0: 组名 = 端点名，显示健康状态
    const healthStatus = activeGroup.healthy_endpoints > 0 ? '✓ 健康' : '✗ 异常';
    activeEndpointText = `${activeGroup.name} (${healthStatus})`;
  } else if (endpoints.healthy > 0) {
    // 回退：如果没有活跃组信息，但有健康端点
    activeEndpointText = `${endpoints.healthy} 个可用`;
  }

  return (
    <div className="grid grid-cols-2 md:grid-cols-5 gap-4 mb-6">
      <KPICard
        title="服务状态"
        value={status.status === 'running' ? '运行中' : '已停止'}
        icon={Zap}
        statusColor={status.status === 'running' ? 'bg-emerald-50 text-emerald-600' : 'bg-rose-50 text-rose-600'}
      />
      <KPICard
        title="运行时间"
        value={status.uptime || '加载中...'}
        icon={Clock}
        statusColor="bg-blue-50 text-blue-600"
      />
      <KPICard
        title="端点数量"
        value={endpoints.total || 0}
        icon={Server}
        statusColor="bg-indigo-50 text-indigo-600"
      />
      <KPICard
        title="总请求数"
        value={connections.all_time_total_requests || 0}
        icon={Network}
        statusColor="bg-purple-50 text-purple-600"
      />
      <KPICard
        title="当前活动端点"
        value={activeEndpointText}
        icon={Activity}
        statusColor={activeGroup ? 'bg-emerald-50 text-emerald-600' : 'bg-slate-50 text-slate-600'}
      />
    </div>
  );
};

export default KPICardsGrid;
