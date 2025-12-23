# 开发记录（v5.1.0 → 当前）

位置：`docs/DEVLOG.md`

本文件用于记录本轮“端点 → 渠道（channel）”重构及相关优化的**详细实现**，便于后续继续迭代、排查回归与快速定位代码。

起始基线：`25e801c8ddf9b2be5c683dd4221e888477dbb8f3`（tag: `v5.1.0`）  
当前 HEAD：`c84a4a7`（建议发布为 `v6.0.1`）  
合并节点：`919e510 Merge branch 'feat/channel-priority'`

---

## 0. 总体结论（一句话）

请求路由以“**渠道**”为第一层边界：优先只在**当前激活渠道**内选择端点；渠道内端点失败按策略切端点；渠道内耗尽后触发**跨渠道故障转移**并按策略选择下一个可用渠道。

---

## 1. 业务语义与规则（必须保持一致）

### 1.1 路由层级

1) 渠道内：在当前激活渠道内选择端点转发  
2) 渠道内故障转移：同渠道端点失败 → 切到同渠道内其他端点  
3) 跨渠道故障转移：当“当前渠道内可用端点全部尝试且重试耗尽” → 切到下一渠道

### 1.2 优先级语义

- 端点 `priority`：仅用于**同渠道内**端点顺序（越小越优先）。  
- 渠道 `priority`：仅用于**跨渠道**切换顺序（越小越优先）；只在启用“渠道间故障转移”时生效。

### 1.3 路由策略 `strategy.type`

- `priority`：
  - 渠道内：按端点 `priority` ASC 选择
  - 渠道间：按渠道 `priority` ASC 选择
- `fastest`：
  - 渠道内：按端点健康检查 `responseTime` ASC（可配合 fast_test）
  - 渠道间：在“可切换渠道集合”中，按“各渠道内可用端点的最小 `responseTime`”排序选取

重要：`fastest` **只用于排序，不用于可用性判定**（避免网络瞬时抖动导致误判“不可用”）。

---

## 2. 核心实现：后端（Go）

### 2.1 端点候选获取（渠道内）

入口：`internal/endpoint/endpoint_selection.go`

关键函数：
- `(*Manager) GetHealthyEndpoints() []*Endpoint`
- `(*Manager) GetFastestEndpointsWithRealTimeTest(ctx) []*Endpoint`
- `(*Manager) sortHealthyEndpoints(healthy, showLogs)`

实现要点：
- 只从当前激活渠道取端点：`GroupManager.FilterEndpointsByActiveGroups(snapshot)`
- 候选端点过滤条件（与“参与渠道内故障转移”强绑定）：
  - `FailoverEnabled != false`（默认参与）
  - `Healthy == true`
  - 不在端点请求冷却：`CooldownUntil` 未到
- 排序：
  - `priority`：`EndpointConfig.Priority` ASC
  - `fastest`：`EndpointStatus.ResponseTime` ASC
  - 若开启 `fast_test_enabled`：并行快测（有缓存 TTL）后再按实测结果排序

### 2.2 渠道状态管理（激活/暂停/冷却）

入口：`internal/endpoint/group_manager.go`

关键点：
- 组名（groupName）在 v6.0 语义下等于：`ChannelKey(ep)`（优先用 `endpoint.Config.Channel`，缺省回退为端点名以兼容旧逻辑）。
- `UpdateGroups(endpoints)` 会：
  - 由端点构建渠道组；
  - 计算组优先级：
    - 默认取“组内最小 endpoint.priority”
    - v6.1+ 若存在渠道优先级映射（`channelPriorities`）则覆盖（用于跨渠道）
  - 组级暂停（ManuallyPaused）判定：**当且仅当组内所有端点 `failover_enabled=false`** 时将该组视为暂停（避免出现“孤儿端点/不可用组”）。
- 激活策略：
  - SQLite 模式下 `enabled` 字段会影响“当前激活渠道”的同步（见 `internal/service/endpoint.go:SyncFromDatabase` 的互斥修正逻辑）。

### 2.3 跨渠道故障转移（请求级）

入口：`internal/endpoint/failover.go`

关键函数：
- `(*Manager) TriggerRequestFailoverWithFailedEndpoints(failedEndpointNames []string, reason string) (string, error)`

触发源（代理层）：
- 常规：`internal/proxy/handlers/regular.go`（当前渠道端点耗尽后触发）
- 流式：`internal/proxy/handlers/streaming.go`（同上）

执行步骤（实现顺序）：
1) 校验 `Failover.Enabled`（该开关语义为“渠道间故障转移”）
2) 去重失败端点列表 → 解析失败渠道 `failedChannel`
3) **对失败端点应用请求冷却**（仅对属于失败渠道的端点；best-effort）
4) **对失败渠道应用渠道冷却**
5) 构造候选渠道集合，过滤不可用渠道（见 2.3.1）
6) 按 `strategy.type` 对候选渠道排序并选取新渠道
7) 激活新渠道（必要时允许 force activation 兼容 neverChecked）
8) 回调通知 App 层同步数据库、触发前端刷新

#### 2.3.1 “候选渠道可用性判定”（跨渠道）

跨渠道的“可用性”判定**不依赖响应时间**，只依赖状态与端点候选规则：
- 跳过：
  - `ManuallyPaused == true` 的渠道
  - `CooldownUntil` 未到的渠道
  - 当前失败渠道本身
- 目标渠道需要满足：至少存在 1 个端点满足：
  - `FailoverEnabled != false`
  - 不在端点请求冷却
  - `(Healthy == true) || (NeverChecked == true)`

#### 2.3.2 “fastest” 的跨渠道排序方式

仅用于排序，不参与可用性：
- 对每个候选渠道，计算 `bestRT = min(responseTime)`（仅统计 `responseTime > 0` 的端点）
- 排序规则：
  1) 有 `bestRT` 的渠道优先于“无响应时间记录”的渠道
  2) `bestRT` ASC
  3) 渠道优先级 ASC（兜底稳定）
  4) 渠道名 ASC（最终稳定兜底）

### 2.4 渠道优先级同步到运行时

链路目标：让 DB 中 `channels.priority` 实时影响运行时 `GroupManager` 的渠道优先级（仅跨渠道使用）。

实现落点（代表性）：
- `internal/store/channel.go`：读取渠道列表携带 `priority`
- `app_api_storage.go` / `app.go`：启动与更新时同步 `channelPriorities` 到 `endpoint.Manager.UpdateChannelPriorities`
- `internal/endpoint/group_manager.go`：`UpdateGroups` 时使用 `channelPriorities` 覆盖组优先级

### 2.5 端点重命名（SQLite 记录）

入口：`app_api_storage.go`（UpdateEndpointRecord）

实现要点：
- 若请求的 `input.name` 与 URL 参数（旧 name）不同：在事务内先更新 `endpoints.name` 再更新其它字段
- best-effort 同步历史表的 `endpoint_name` 显示字段（避免请求追踪/用量 UI 由于 name 变化查不到历史记录）
- 因运行时 `UpdateEndpointConfig` 不支持“改名”，完成后会触发 `SyncFromDatabase` 重建运行时端点列表（KISS：避免复杂的运行时 rename 操作）

---

## 3. 核心实现：前端（Wails/React）

### 3.1 渠道管理页面（原 endpoints 页面语义升级）

主页面：`frontend/src/pages/endpoints/index.jsx`

实现要点：
- 以“渠道卡片”分块展示：大屏双列布局（两渠道同一行）
- 渠道卡片：
  - 展示：名称、官网、优先级、激活状态（高亮/置顶）
  - 操作：激活、暂停/恢复、编辑渠道、删除渠道、渠道内新增端点
  - 端点展示：精简卡片（限制默认展示数量，多余通过展开/查看全部）
- 渠道删除：
  - 当渠道仍有端点时，提示“该渠道下仍有 N 个端点；确认删除将级联删除端点并删除渠道”
  - 取消了多余的“勾选才删除端点”选项

### 3.5 页面标题区 UI 统一（图标 + 标题 + 副标题）

背景：请求追踪/系统日志/系统设置标题旁有图标，但其他页面缺失，且系统日志标题区的图标与文案对齐不一致。

处理：统一所有主要页面的标题区样式：左侧固定图标块 + 右侧标题/副标题文本，保证视觉一致性与对齐。

实现落点：
- `frontend/src/pages/log-viewer/index.jsx`：调整标题结构，确保“系统日志”与“实时查看系统运行日志 · 共 N 条”同一垂直线对齐，并统一图标块样式。
- `frontend/src/pages/overview/index.jsx`：概览页补齐标题图标与统一布局。
- `frontend/src/pages/pricing/index.jsx`：基础定价页补齐标题图标与统一布局。
- `frontend/src/pages/endpoints/index.jsx`：渠道管理页补齐标题图标与统一布局。

### 3.2 端点详情弹窗（美化 + 实用性）

实现落点：`frontend/src/pages/endpoints/index.jsx`（`EndpointDetailModal`）

关键交互：
- 点击遮罩（卡片外区域）关闭；点击卡片内部不关闭
- Token/API Key：
  - 默认展示脱敏值（或动态遮罩）
  - 提供“复制原始值”的小图标按钮（仅原始值存在时可点）
- 布局：
  - 冷却(s) 与 Token（脱敏）保持同一行的双列卡片布局（小窗口自动换行）
  - 当端点未配置独立冷却(s)时，详情中展示“全局默认冷却时间”（避免显示 `-` 造成误解）

### 3.3 端点表单（新建/编辑）

实现落点：`frontend/src/pages/endpoints/components/EndpointForm.jsx`

实现要点：
- 渠道字段：
  - 编辑时：下拉选择已有渠道（做了样式美化，替代原生难看箭头）
  - 新建时：可通过“从渠道卡片新增端点”锁定默认渠道
- 端点名称：
  - 编辑模式允许修改（与后端 rename 支持配套）
- 开关项 UI：
  - “参与渠道内故障转移”“支持 count_tokens”使用 switch 卡片风格

### 3.4 设置页：鉴权 Token 脱敏展示 + 显示/隐藏

后端元数据：`internal/service/settings.go` 将 `auth.token` 的 `ValueType` 标记为 `password`  
前端渲染：`frontend/src/pages/settings/components/SettingItem.jsx`

实现要点：
- 默认 `password` 输入类型脱敏显示
- 右侧 Eye/EyeOff 图标切换显示/隐藏
- Token 输入框加宽，便于查看/编辑

---

## 4. 数据库迁移与兼容性（详细）

### 4.1 迁移与 schema
- schema 定义：`internal/tracking/schema.sql`
- migration/列检查与 ALTER：`internal/tracking/sqlite_adapter.go`

关键变更：
- `channels` 增加 `priority INTEGER DEFAULT 1`
- 读取渠道列表时按稳定排序：`priority ASC, created_at DESC, name ASC`（store 层）
- 说明：本版本以“端点”为统计/追踪维度，不引入“按 Token/API Key 聚合统计”的额外字段与汇总表（该需求已废案）。

### 4.2 时间字段解析兼容

文件：`internal/store/sqlite_time.go`

目的：
- 兼容历史时间格式差异导致的“创建时间为空/排序乱/新建不置顶”等问题。

---

## 5. 设置项语义与联动（重要）

### 5.1 “启用渠道间故障转移”（Failover.Enabled）

当前语义：
- 仅控制**跨渠道**切换（请求级触发）与渠道自动切换行为；
- 不影响渠道内端点轮换：渠道内端点轮换默认始终开启，单个端点可用 `failover_enabled=false` 退出候选。

实现落点：
- 描述文案：`internal/service/settings.go`
- 跨渠道切换入口：`internal/endpoint/failover.go`
- 渠道内候选过滤：`internal/endpoint/endpoint_selection.go`

### 5.2 “路由策略”（Strategy.Type）

当前语义：
- 渠道内：端点排序/选择策略（priority/fastest）
- 渠道间：启用跨渠道故障转移时，“下一渠道选择”也遵循该策略（但 fastest 仅排序，不判定可用性）

实现落点：
- 设置默认项说明：`internal/service/settings.go`
- 示例配置注释：`config/example.yaml`

---

## 6. 提交时间线（从 v5.1.0 起，含合并）

> 来源：`git log 25e801c..HEAD --oneline`（仅列本轮范围内）。

1) `fabdd3a` feat: EOF 重试提示 + 端点渠道分组显示  
2) `23fb80a` fix: 改进 EOF 重试机制，使用双 message_start 模式触发客户端自动重试  
3) `e1b74a5` refactor(endpoint): 重构故障转移为渠道级路由机制  
4) `6a05f89` chore: release v5.2.1  
5) `d45e5e2` fix: 修复 SuspensionManager 配置热更新未生效的问题  
6) `365da00` fix: 修复 EOF 重试机制因 Flusher 接口断言失败导致失效  
7) `9f6c372` chore: release v5.2.3  
8) `0fae9e7` feat(failover,channels): 优化故障转移逻辑并新增渠道管理功能  
9) `c72c38a` fix(database): 优化数据库迁移逻辑与并发安全性  
10) `eb0ed9e` feat(channel): 实现渠道优先级与编辑功能  
11) `919e510` Merge branch 'feat/channel-priority'  
12) `f5724b5` feat(endpoint): 支持端点重命名和渠道选择器优化  
13) `15206c8` refactor(frontend): 优化端点表单复选框交互和详情展示  
14) `853339a` feat(failover): 支持跨渠道故障转移的路由策略选择  
15) `c84a4a7` feat(settings): 为鉴权 Token 添加密码类型和可见性切换  

---

## 7. 变更明细（按提交拆解，贴近实现与文件）

> 说明：本节按提交聚合“做了什么/为什么/怎么做/改了哪些文件”，方便快速回溯。

### 7.1 `fabdd3a` feat: EOF 重试提示 + 端点渠道分组显示
- 做了什么：
  - 引入 EOF 场景可重试提示；前端开始支持端点按“渠道”分组展示。
- 怎么做：
  - 流式处理增加 EOF 处理/测试；
  - 前端引入渠道行、删除确认、工具函数等基础组件。
- 代表文件：
  - `internal/proxy/handlers/streaming.go`
  - `frontend/src/pages/endpoints/index.jsx`
  - `frontend/src/pages/endpoints/components/*`

### 7.2 `23fb80a` fix: 双 message_start EOF 重试提示
- 做了什么：提升 EOF 重试提示触发的稳定性与兼容性。
- 代表文件：`internal/proxy/handlers/streaming.go`

### 7.3 `e1b74a5` refactor(endpoint): 渠道级路由机制
- 做了什么：将“路由/故障转移”的第一层边界调整为渠道。
- 怎么做（关键路径）：
  - `GroupManager` 以 `ChannelKey(ep)` 作为组名；
  - `GetHealthyEndpoints` 只返回当前激活渠道的端点；
  - 跨渠道切换改为请求级触发入口（后续在 `0fae9e7/853339a` 补强）。
- 代表文件：
  - `internal/endpoint/endpoint_selection.go`
  - `internal/endpoint/group_manager.go`
  - `internal/endpoint/failover.go`

### 7.4 `0fae9e7` feat(failover,channels): 渠道管理功能落地
- 做了什么：
  - 渠道管理 UI 落地；故障转移与挂起、自愈等路径补强。
- 怎么做：
  - 代理层在渠道耗尽后调用请求级故障转移；
  - 前端渠道卡片/端点精简卡片/详情弹窗逐步成型；
  - 设置项文案与行为联动补齐。
- 代表文件：
  - `internal/proxy/handlers/regular.go`
  - `internal/proxy/handlers/streaming.go`
  - `internal/endpoint/failover.go`
  - `frontend/src/pages/endpoints/index.jsx`

### 7.5 `c72c38a` fix(database): 迁移逻辑与并发安全
- 做了什么：修复迁移/并发导致的“创建超时、UI 延迟显示、加载失败”等典型问题根源。
- 怎么做：
  - 强化 migration 检查、ALTER 容错、查询稳定性；
  - 前端在瞬时失败时避免清空数据（保留上一次结果）。
- 代表文件：
  - `internal/tracking/sqlite_adapter.go`
  - `internal/tracking/queries.go`
  - `app_api_storage.go`

### 7.6 `eb0ed9e` feat(channel): 渠道优先级 + 渠道编辑（并含品牌/打包信息统一）
- 做了什么：
  - 引入 `channels.priority`，并接入运行时与 UI；
  - 增加“编辑渠道”（官网/优先级）；
  - 同步部分品牌/窗口标题/Windows 资源信息（打包展示一致性）。
- 怎么做：
  - DB：增加 `priority` 列 + store 层稳定排序；
  - 运行时：同步 priority map 到 `GroupManager`；
  - 前端：`createChannel` 增加 priority；新增 `updateChannel`。
- 代表文件：
  - `internal/store/channel.go`
  - `internal/endpoint/group_manager.go`
  - `internal/service/channel.go`
  - `frontend/src/utils/wailsApi.js`
  - `main.go`、`wails.json`、`build/windows/info.json`、`frontend/index.html`

### 7.7 `f5724b5` feat(endpoint): 端点重命名 + 渠道选择器
- 做了什么：
  - 端点可重命名；
  - 编辑端点时渠道改为下拉选择，并允许修改端点名称。
- 怎么做：
  - 后端：事务内改名 + best-effort 同步历史展示字段 + `SyncFromDatabase` 重建运行时；
  - 前端：渠道 select 美化；名称字段可编辑。
- 代表文件：
  - `app_api_storage.go`
  - `frontend/src/pages/endpoints/components/EndpointForm.jsx`

### 7.8 `15206c8` refactor(frontend): 表单/详情体验优化
- 做了什么：
  - 表单 switch 风格开关；
  - 详情弹窗布局优化；点击遮罩关闭；
  - Token/API Key 复制原始值按钮；
  - 清理无用“复制配置”入口。
- 代表文件：
  - `frontend/src/pages/endpoints/index.jsx`
  - `frontend/src/pages/endpoints/components/EndpointForm.jsx`
  - `frontend/src/pages/endpoints/components/EndpointRow.jsx`

### 7.9 `853339a` feat(failover): 跨渠道切换也遵循 strategy
- 做了什么：
  - 跨渠道故障转移的“下一渠道选择”纳入 `strategy.type = priority|fastest`。
- 怎么做：
  - 候选渠道“可用性判定”与“fastest 排序”严格分离；
  - 补充单测覆盖 fastest 场景与“跳过暂停渠道”场景。
- 代表文件：
  - `internal/endpoint/failover.go`
  - `internal/endpoint/failover_strategy_test.go`
  - `internal/service/settings.go`
  - `config/example.yaml`

### 7.10 `c84a4a7` feat(settings): 鉴权 Token 脱敏 + 显示/隐藏
- 做了什么：设置页 `auth.token` 默认脱敏展示，并提供显示/隐藏切换。
- 怎么做：
  - 后端：`auth.token` value_type 改为 `password`；
  - 前端：对 `password` 类型渲染为带 Eye/EyeOff 的输入框，并加宽。
- 代表文件：
  - `internal/service/settings.go`
  - `frontend/src/pages/settings/components/SettingItem.jsx`

### 7.11 fix(sqlite): 规避 SQLITE_BUSY 导致设置/定价空白
- 现象：运行一段时间后，设置页偶发报错 `database is locked (SQLITE_BUSY)`；模型定价页面可能显示为空。
- 根因：SQLite 的 `VACUUM` 属于强锁操作（Windows 上尤甚），在后台自动执行时会把其它连接的读写顶成 `SQLITE_BUSY`。
- 怎么做：
  - 停止在“定期清理”流程中自动执行 `VACUUM`（按需再提供手动维护入口，避免运行时锁库）。
  - 管理侧查询加入轻量 `SQLITE_BUSY` 重试（在 ctx 允许的时间内退避等待，避免 UI 直接变空）。
- 代表文件：
  - `internal/tracking/database.go`
  - `internal/store/sqlite_busy_retry.go`
  - `internal/store/settings.go`
  - `internal/store/model_pricing.go`

---

## 8. 验证与回归（建议保留的命令）

后端：
- `go build .`
- `go test ./internal/endpoint -run TestTriggerRequestFailoverWithFailedEndpoints_ -count=1`

前端：
- `cd frontend && npm run build`

说明：
- 全量 `go test ./...` 可能因历史 integration tests 与平台差异（cgo/toolchain/旧字段）失败，和本轮变更不必然相关；建议针对改动模块运行对应单测集即可。

---

## 9. 已废弃/移除（避免后续重复引入）

### 9.1 按 Token 聚合统计（废案）

结论：
- “同渠道多端点不同 Token 可用性验证”是运行时行为验证；统计与展示维度以端点为准即可。
- 按 Token/API Key 聚合会与端点维度高度重合，并引入额外 schema/migration/兼容复杂度，属于冗余（YAGNI）。

已移除（代表性落点）：
- 请求追踪：不再展示“认证”字段（仅保留端点/渠道/模型/状态等信息）。
- 数据库 schema：不再创建 `usage_summary_by_auth`，也不再要求 `request_logs` 拥有认证维度列。
