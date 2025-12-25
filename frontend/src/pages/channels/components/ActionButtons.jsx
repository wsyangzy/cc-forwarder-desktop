// ============================================
// 操作按钮组件
// 端点行操作按钮（激活组、更新优先级）
// 2025-11-28
// ============================================

import { useState } from 'react';
import { Zap, Save } from 'lucide-react';

/**
 * 操作按钮组件
 * @param {Object} endpoint - 端点数据
 * @param {Function} onActivateGroup - 激活组回调
 * @param {Object} priorityEditorRef - 优先级编辑器 ref
 * @param {boolean} disabled - 是否禁用
 */
const ActionButtons = ({
  endpoint,
  onActivateGroup,
  priorityEditorRef,
  disabled = false
}) => {
  const [activateLoading, setActivateLoading] = useState(false);

  // 判断是否有有效的组配置
  const hasValidGroup = endpoint.group && endpoint.group !== 'default';
  // 判断组是否已经激活
  const groupIsActive = endpoint.group_is_active;
  // 按钮是否可用：有效组 且 组未激活
  const canActivate = hasValidGroup && !groupIsActive;

  // 激活组处理
  const handleActivateGroup = async () => {
    if (!canActivate || !onActivateGroup) return;

    setActivateLoading(true);
    try {
      await onActivateGroup(endpoint.name, endpoint.group);
    } catch (error) {
      console.error('激活组失败:', error);
      alert(`激活组失败: ${error.message}`);
    } finally {
      setActivateLoading(false);
    }
  };

  // 更新优先级处理
  const handleUpdatePriority = async () => {
    if (!priorityEditorRef?.current) return;

    try {
      await priorityEditorRef.current.executeUpdate();
    } catch (error) {
      console.error('更新优先级失败:', error);
    }
  };

  return (
    <div className="flex items-center gap-1.5">
      {/* 激活组按钮 */}
      <button
        onClick={handleActivateGroup}
        disabled={disabled || activateLoading || !canActivate}
        className={`
          inline-flex items-center gap-1 px-2 py-1 text-xs font-medium rounded transition-all
          ${canActivate
            ? 'bg-emerald-50 text-emerald-600 hover:bg-emerald-100 border border-emerald-200'
            : groupIsActive
              ? 'bg-emerald-500 text-white border border-emerald-500'
              : 'bg-slate-50 text-slate-400 border border-slate-200 cursor-not-allowed'}
          ${activateLoading ? 'opacity-50' : ''}
        `}
        title={
          !hasValidGroup ? '端点未配置组信息' :
          groupIsActive ? `组 "${endpoint.group}" 已启用` :
          `启用组: ${endpoint.group}`
        }
      >
        <Zap size={12} />
        {activateLoading ? '启用中' : groupIsActive ? '活跃' : '启用'}
      </button>

      {/* 更新优先级按钮 */}
      <button
        onClick={handleUpdatePriority}
        disabled={disabled || (priorityEditorRef?.current?.isUpdating)}
        className="
          inline-flex items-center gap-1 px-2 py-1 text-xs font-medium rounded
          bg-indigo-50 text-indigo-600 hover:bg-indigo-100 border border-indigo-200
          transition-all disabled:opacity-50 disabled:cursor-not-allowed
        "
        title="保存优先级更改"
      >
        <Save size={12} />
        更新
      </button>
    </div>
  );
};

export default ActionButtons;
