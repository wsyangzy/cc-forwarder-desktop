// ============================================
// DeleteConfirmDialog - 删除确认对话框
// ============================================

import React from 'react';
import { AlertTriangle, Trash2 } from 'lucide-react';
import { Button } from '@components/ui';

const DeleteConfirmDialog = ({ endpoint, onConfirm, onCancel, loading }) => (
  <div className="fixed inset-0 bg-black/50 flex items-start justify-center z-50 animate-fade-in pt-[20vh]">
    <div className="bg-white rounded-2xl shadow-xl w-full max-w-md p-6">
      <div className="flex items-center gap-3 mb-4">
        <div className="p-3 bg-rose-100 rounded-full">
          <AlertTriangle className="text-rose-600" size={24} />
        </div>
        <div>
          <h3 className="text-lg font-semibold text-slate-900">确认删除</h3>
          <p className="text-sm text-slate-500">此操作不可撤销</p>
        </div>
      </div>

      <p className="text-slate-700 mb-6">
        确定要删除端点 <span className="font-semibold">&quot;{endpoint?.name}&quot;</span> 吗？
        删除后将无法恢复。
      </p>

      <div className="flex justify-end gap-3">
        <Button variant="ghost" onClick={onCancel} disabled={loading}>
          取消
        </Button>
        <Button
          variant="danger"
          icon={Trash2}
          onClick={onConfirm}
          loading={loading}
        >
          确认删除
        </Button>
      </div>
    </div>
  </div>
);

export default DeleteConfirmDialog;
