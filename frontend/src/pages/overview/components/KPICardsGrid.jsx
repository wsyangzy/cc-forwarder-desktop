// ============================================
// KPI 卡片网格组件
// 2025-12-06 v4.0: 一个端点 = 一个组
// 2025-12-12 v5.1: 服务状态改为成本和 tokens 统计
// ============================================

import {
  DollarSign,
  Zap,
  Server,
  Network,
  Activity
} from 'lucide-react';
import { KPICard } from '@components/ui';

// 格式化成本显示
const formatCost = (cost) => {
  if (!cost || cost === 0) return '$0.00';
  if (cost >= 1) return `$${cost.toFixed(2)}`;
  if (cost >= 0.01) return `$${cost.toFixed(3)}`;
  return `$${cost.toFixed(4)}`;
};

// 格式化 tokens 显示
const formatTokens = (tokens) => {
  if (!tokens || tokens === 0) return '0';
  if (tokens >= 1000000) return `${(tokens / 1000000).toFixed(2)}M`;
  if (tokens >= 1000) return `${(tokens / 1000).toFixed(1)}K`;
  return tokens.toString();
};

const KPICardsGrid = ({ data }) => {
  const { status, endpoints, connections, groups } = data;

  // v4.0: 一个端点 = 一个组，显示当前活动端点
  // 优先从 groups 获取活跃状态，因为 group.is_active 才表示"正在使用"
  const activeGroup = groups.groups?.find(g => g.is_active);

  // 构建显示文本
  let activeEndpointText = '无活动端点';
  let activeEndpointStatusColor = 'bg-slate-50 text-slate-600';
  let activeEndpointTooltip = activeGroup ? activeGroup.name : '无活动端点';
  if (activeGroup) {
    // v6.0+: 端点健康三态：健康/异常/未检测（未检测=灰色）
    const activeGroupName = activeGroup.name;
    const list = endpoints.endpoints || [];
    const inGroup = list.filter((e) => (e.group || e.channel || e.name) === activeGroupName);
    const checked = inGroup.filter((e) => {
      const neverChecked = e.never_checked || e.neverChecked;
      const hasLastCheck = !!(e.last_check || e.lastCheck);
      return !neverChecked && hasLastCheck;
    });
    const healthyCount = checked.filter((e) => e.status === 'healthy' || e.healthy).length;

    const healthStatus = checked.length === 0
      ? '未检测'
      : (healthyCount > 0 ? '✓ 健康' : '✗ 异常');

    activeEndpointText = `${activeGroupName} (${healthStatus})`;
    activeEndpointTooltip = activeEndpointText;
    activeEndpointStatusColor = checked.length === 0
      ? 'bg-slate-50 text-slate-600'
      : (healthyCount > 0 ? 'bg-emerald-50 text-emerald-600' : 'bg-rose-50 text-rose-600');
  } else if (endpoints.healthy > 0) {
    // 回退：如果没有活跃组信息，但有健康端点
    activeEndpointText = `${endpoints.healthy} 个可用`;
    activeEndpointTooltip = activeEndpointText;
    activeEndpointStatusColor = 'bg-emerald-50 text-emerald-600';
  }

  // v5.1+: 成本和 tokens 数据
  const todayCost = formatCost(connections.today_cost);
  const totalCost = formatCost(connections.all_time_total_cost);
  const todayTokens = formatTokens(connections.today_tokens);
  const totalTokens = formatTokens(connections.all_time_total_tokens);

  return (
    <div className="grid grid-cols-2 md:grid-cols-4 xl:grid-cols-8 gap-4 mb-6">
      <KPICard
        title="今日成本"
        value={todayCost}
        icon={DollarSign}
        statusColor="bg-emerald-50 text-emerald-600"
      />
      <KPICard
        title="今日 Tokens"
        value={todayTokens}
        icon={Zap}
        statusColor="bg-amber-50 text-amber-600"
      />
      <KPICard
        title="今日请求"
        value={connections.today_requests || 0}
        icon={Network}
        statusColor="bg-cyan-50 text-cyan-600"
      />
      <KPICard
        title="总成本"
        value={totalCost}
        icon={DollarSign}
        statusColor="bg-blue-50 text-blue-600"
      />
      <KPICard
        title="总 Tokens"
        value={totalTokens}
        icon={Zap}
        statusColor="bg-indigo-50 text-indigo-600"
      />
      <KPICard
        title="总请求数"
        value={connections.all_time_total_requests || 0}
        icon={Network}
        statusColor="bg-purple-50 text-purple-600"
      />
      <KPICard
        title="端点数量"
        value={endpoints.total || 0}
        icon={Server}
        statusColor="bg-violet-50 text-violet-600"
      />
      <KPICard
        title="活动端点"
        value={activeEndpointText}
        tooltip={activeEndpointTooltip}
        icon={Activity}
        statusColor={activeEndpointStatusColor}
      />
    </div>
  );
};

export default KPICardsGrid;
