// ============================================
// GroupTableRow - List 视图组表格行组件
// 2025-12-01
// ============================================

import { Play, Pause, Clock, Activity, Server, Settings2, Power } from 'lucide-react';

const GroupTableRow = ({ group, onActivate, onPause, loading, channelFailoverEnabled = true }) => {
  const isActive = group.is_active;
  const isCooldown = group.in_cooldown;
  const healthyPercent = group.total_endpoints > 0
    ? Math.round((group.healthy_endpoints / group.total_endpoints) * 100)
    : 0;

  return (
    <tr
      className={`group hover:bg-gray-50 transition-colors ${
        isActive ? 'bg-indigo-50/30 border-l-4 border-l-indigo-500' : 'border-l-4 border-l-transparent'
      }`}
    >
      {/* 分组名称 & 优先级 */}
      <td className="px-6 py-4">
        <div className="flex items-center gap-2">
          <div className="font-bold text-gray-900 text-base">{group.name}</div>
          {isActive && (
            <span className="flex h-2 w-2 rounded-full bg-indigo-500 ring-4 ring-indigo-100" />
          )}
        </div>
        <div className="mt-1 flex items-center gap-2">
          <span className="text-xs text-gray-500 px-1.5 py-0.5 bg-gray-100 rounded border border-gray-200">
            Priority {group.priority}
          </span>
          {isCooldown && (
            <span className="text-xs text-amber-600 px-1.5 py-0.5 bg-amber-50 rounded border border-amber-200 flex items-center gap-1">
              <Clock size={10} /> {group.cooldown_remaining || '冷却中'}
            </span>
          )}
        </div>
      </td>

      {/* 状态 */}
      <td className="px-6 py-4">
        {isActive ? (
          <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-semibold bg-indigo-100 text-indigo-700 border border-indigo-200">
            <Activity className="w-3.5 h-3.5" /> 活跃中
          </span>
        ) : isCooldown ? (
          <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium bg-amber-100 text-amber-700 border border-amber-200">
            <Clock className="w-3.5 h-3.5" /> 冷却中
          </span>
        ) : (
          <span className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium bg-gray-100 text-gray-500 border border-gray-200">
            <Power className="w-3.5 h-3.5" /> 待机
          </span>
        )}
      </td>

      {/* 健康度指标 */}
      <td className="px-6 py-4">
        <div className="flex items-center gap-3 w-full max-w-[180px]">
          <div className="flex-1 h-2 bg-gray-100 rounded-full overflow-hidden">
            <div
              className={`h-full rounded-full ${
                healthyPercent === 100 ? 'bg-emerald-500' :
                healthyPercent === 0 ? 'bg-transparent' : 'bg-amber-500'
              }`}
              style={{ width: `${healthyPercent}%` }}
            />
          </div>
          <span className={`text-sm font-bold font-mono ${
            healthyPercent === 100 ? 'text-emerald-600' : 'text-amber-600'
          }`}>
            {healthyPercent}%
          </span>
        </div>
      </td>

      {/* 端点概览 */}
      <td className="px-6 py-4">
        <div className="flex items-center gap-2 text-gray-600">
          <Server className="w-4 h-4 text-gray-400" />
          <span className="font-medium">{group.healthy_endpoints}</span>
          <span className="text-gray-400">/ {group.total_endpoints}</span>
        </div>
      </td>

      {/* 操作 */}
      <td className="px-6 py-4 text-right">
        <div className="flex items-center justify-end gap-2 opacity-0 group-hover:opacity-100 transition-opacity">
          <button className="p-2 text-gray-400 hover:text-indigo-600 hover:bg-indigo-50 rounded-lg transition-colors">
            <Settings2 className="w-4 h-4" />
          </button>
          {isActive ? (
            <button
              onClick={() => onPause(group.name)}
              disabled={loading || !channelFailoverEnabled}
              title={!channelFailoverEnabled ? '已关闭渠道间故障转移：不可暂停渠道' : undefined}
              className="px-3 py-1.5 text-xs font-medium bg-rose-600 text-white rounded-lg hover:bg-rose-700 transition-colors flex items-center gap-1 shadow-sm disabled:opacity-50"
            >
              <Pause className="w-3.5 h-3.5" /> 暂停
            </button>
          ) : (
            <button
              onClick={() => onActivate(group.name)}
              disabled={loading || isCooldown}
              className="px-3 py-1.5 text-xs font-medium bg-slate-900 text-white rounded-lg hover:bg-slate-800 transition-colors flex items-center gap-1 shadow-sm disabled:opacity-50 disabled:cursor-not-allowed"
            >
              <Play className="w-3.5 h-3.5" /> 激活
            </button>
          )}
        </div>
      </td>
    </tr>
  );
};

export default GroupTableRow;
