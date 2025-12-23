# TODO（后续开发任务清单）

本文件用于记录下一阶段待验证/待实现事项，避免口头需求丢失，并便于按模块拆解开发与验收。

---

## 1) 渠道内多端点：不同 Token 可用性验证

### 背景
- 当前路由单位为“渠道”，渠道内可有多个端点参与故障转移。
- 端点可能配置不同的 Token。
- 需要确认“同渠道多端点、不同 Token”在真实请求与故障转移链路下是否可用（端点切换时 `Authorization` 是否随端点切换）。

### 目标
1. 验证：同一渠道内多个端点配置不同 Token，路由/故障转移/重试链路均能正常工作。
2. 结论：按 Token 聚合统计属于冗余需求（废案）；统计维度以端点为准。

### 任务拆解（建议顺序）
1. 路由/故障转移行为验证（不改代码的验证项）
   - 同渠道 2 个端点：
     - endpoint-A：Token-A
     - endpoint-B：Token-B
   - 验证点：
     - priority 策略下：优先使用 endpoint-A，失败后切到 endpoint-B
     - fastest 策略下：按延迟选择，失败后仍能切到另一端点
     - 端点 failover_enabled=false 时：不参与候选与故障转移
     - 端点冷却/渠道冷却生效后：候选过滤正确
2. 验证手段建议（任选其一）
   - 方案 A（推荐）：用两个可控的上游服务/Mock Server 记录收到的 `Authorization`，并在故障转移时确认 token 随端点变化。
   - 方案 B：通过上游的访问日志/控制台（若可见）确认不同端点的鉴权凭据分别生效。
3. 统计确认（仅验证）
   - 请求追踪/概览/用量统计：维度以端点为准，不按 token 聚合。

### 验收标准
- 同渠道多端点不同 Token 在实际请求与故障转移中可用（无异常 401/签名错误等非预期问题）。
- 验证在 priority / fastest 路由策略下，端点切换行为一致且稳定。

### 风险/注意事项
- 若上游对 token 生效有强一致限制（例如白名单/绑定 IP），需要排除上游策略导致的误判。
- 若发现“切换端点后仍使用旧 token”，优先排查端点配置是否被正确加载/热更新。

---

## 2) 支持 OpenAI/Codex 请求转发（OpenAI 兼容 API）

### 背景
- 当前主要适配 Claude 请求链路。
- 需要支持 OpenAI 兼容请求（用户称 “Codex 请求”），以便同一转发器同时处理 Claude/OpenAI 两类请求。

### 目标
1. 后端代理支持 OpenAI 兼容 API 的转发（至少覆盖常用接口与流式）。
2. 前端配置/UI 增补必要字段与提示，确保用户可配置 OpenAI 端点并使用。

### 范围建议（第一阶段）
1. REST 接口（建议优先级从高到低）
   - `POST /v1/chat/completions`（含 stream）
   - `POST /v1/responses`（含 stream，若目标是新协议）
   - `GET /v1/models`（用于健康检查/fastest fast_test_path）
2. 认证
   - 支持 `Authorization: Bearer <api_key>`（复用现有 ApiKey 字段）
3. 计费/统计
   - 记录模型名、输入/输出 token（若响应返回 usage 字段则优先使用）
   - 定价体系：在“基础定价”中支持 OpenAI 模型（可先手动配置）

### 任务拆解（建议顺序）
1. 明确协议分流规则（路由层）
   - 判定请求属于 Claude 还是 OpenAI：
     - 以 path（`/v1/chat/completions`、`/v1/responses` 等）为主
     - 必要时以 header/body 的字段特征兜底
2. 代理层实现（后端）
   - 对 OpenAI 的请求做：
     - request body 透传（尽量不做字段改写，避免破坏兼容）
     - streaming SSE 透传（正确处理 flush/断流/EOF）
   - 错误分类与重试策略：
     - 401/403 不应重试同端点
     - 429 限流按策略退避，可切端点/切渠道（遵循现有重试/故障转移语义）
3. Token 统计/usage 提取
   - 非流式：从响应 JSON 的 `usage` 提取
   - 流式：若流中包含 usage 事件则提取，否则按现有估算/关闭计费（需明确策略）
4. 健康检查/fastest 快测适配
   - OpenAI 端点健康检查：默认 `/v1/models`（或可配置）
   - 确认 fastest fast_test_path 在 OpenAI 端点可用
5. 前端配置与提示
   - 在端点表单中增加“端点类型/协议”（Claude/OpenAI）
   - 根据类型提示用户填写 Token/ApiKey 的正确方式
   - 请求追踪页展示协议类型，便于排查
6. 测试与验收
   - 单测：OpenAI 响应 usage 提取、错误分类、stream 透传
   - 集成测试（最小）：mock OpenAI server + 发起 chat.completions 请求，验证转发与统计

### 验收标准
- OpenAI 兼容接口在非流式/流式场景可正常转发（返回体与状态码正确透传）。
- 在启用重试/故障转移时，OpenAI 场景不引入额外重复计费风险（尤其是流式）。

### 风险/注意事项
- OpenAI streaming 协议与 Claude 的事件格式不同，不能复用 Claude 的流事件解析逻辑；应以“透传优先，解析尽量少”为原则。
- 若要做计费统计，必须优先使用服务端返回 usage，缺失时再考虑估算。
