# CLAUDE.md

本文件为 Claude Code 提供项目指导信息。

## 项目概述

**CC-Forwarder Desktop** 是一款基于 Wails 构建的跨平台桌面应用，用于 Claude API 请求的智能转发、负载均衡和故障恢复。

- **当前版本**: v6.0.3
- **技术栈**: Go + Wails v2 + React + Vite
- **支持平台**: macOS / Windows / Linux

### 核心功能

- 多端点智能路由与优先级调度
- 自动故障转移与端点自愈
- 请求生命周期追踪与状态管理
- 流式响应处理（SSE）
- 内存热池架构优化数据库写入
- SQLite/MySQL 双数据库支持

## 快速开始

```bash
# 开发模式运行
wails dev

# 构建当前平台
wails build

# 构建 Windows 版本（交叉编译）
wails build -platform windows/amd64

# 构建 macOS 版本
wails build -platform darwin/amd64

# 运行测试
go test ./...
```

## 项目结构

```
cc-forwarder-desktop/
├── main.go                 # Wails 应用入口
├── app.go                  # 主应用逻辑，绑定前端 API
├── app_api_*.go           # 前端 API 接口模块
├── wails.json             # Wails 配置
├── frontend/              # React 前端
│   ├── src/
│   │   ├── pages/         # 页面组件
│   │   │   ├── overview/  # 概览页面
│   │   │   ├── endpoints/ # 端点管理
│   │   │   ├── requests/  # 请求追踪
│   │   │   ├── settings/  # 设置页面
│   │   │   └── ...
│   │   └── wailsjs/       # Wails 自动生成的绑定
│   └── dist/              # 构建输出
├── internal/              # 核心业务逻辑
│   ├── proxy/             # 请求转发引擎
│   │   ├── handler.go     # HTTP 请求协调器
│   │   ├── handlers/      # 专用处理器（streaming/regular）
│   │   ├── response/      # 响应处理与 Token 分析
│   │   ├── lifecycle_manager.go  # 状态机生命周期管理
│   │   └── error_recovery.go     # 智能错误恢复
│   ├── endpoint/          # 端点管理与健康检查
│   ├── tracking/          # 使用量追踪与数据库
│   │   ├── hot_pool.go    # 内存热池
│   │   ├── archive_manager.go    # 归档管理器
│   │   └── *_adapter.go   # 数据库适配器（SQLite/MySQL）
│   ├── events/            # 事件系统
│   ├── logging/           # 日志系统
│   └── service/           # 代理服务
├── config/                # 配置文件
│   ├── config.yaml        # 主配置
│   └── example*.yaml      # 配置示例
├── build/                 # 构建相关资源
│   ├── bin/               # 编译输出
│   ├── windows/           # Windows 资源
│   └── darwin/            # macOS 资源
└── docs/                  # 文档
```

## 核心架构

### Wails 应用架构

```
┌─────────────────────────────────────────────────────┐
│                   Wails Runtime                      │
├─────────────────────────────────────────────────────┤
│  Frontend (React)          │    Backend (Go)        │
│  ├── Pages                 │    ├── App Struct      │
│  ├── Components            │    ├── Proxy Service   │
│  └── Wails Bindings  ←────→│    ├── Tracking        │
│                            │    └── Endpoint Mgr    │
└─────────────────────────────────────────────────────┘
```

### 请求处理流程

```
请求接收 → 端点选择 → 转发处理 → 响应分析 → 状态更新
    ↓         ↓          ↓          ↓          ↓
  Pending → Forwarding → Streaming → Processing → Completed
                    ↓
              错误恢复 → 重试/故障转移
```

### 状态机

```
业务状态: pending → forwarding → streaming → processing → completed/failed/cancelled
错误状态: retry / suspended（与业务状态独立）
```

## 配置说明

主配置文件: `config/config.yaml`

```yaml
# 代理服务
proxy:
  host: "127.0.0.1"
  port: 9090

# 端点配置
endpoints:
  - name: "primary"
    url: "https://api.anthropic.com"
    priority: 1
    failover_enabled: true
    token: "sk-xxx"

  - name: "backup"
    url: "https://api.example.com"
    priority: 2
    failover_enabled: true
    token: "sk-yyy"

# 故障转移
failover:
  enabled: true
  default_cooldown: "10m"

# 内存热池（可选）
hot_pool:
  enabled: true
  max_active_requests: 1000
```

## 开发命令

```bash
# 前端开发
cd frontend && npm run dev
```

### 测试策略（重要）

**⚠️ 不要直接运行 `go test ./...`！** 集成测试包含大量 `time.Sleep` 等待（累计 20-30 秒），会严重拖慢开发效率。

```bash
# ✅ 推荐：只运行修改相关的单元测试
go test ./internal/proxy/...      # 代理模块
go test ./internal/endpoint/...   # 端点管理
go test ./internal/tracking/...   # 使用量追踪
go test ./config/...              # 配置模块

# ✅ 运行特定测试函数
go test ./internal/proxy/... -run "TestErrorRecovery"

# ⏳ 集成测试（耗时较长，提交前或 CI 运行）
go test ./tests/integration/...

# 带竞态检测（完整测试时使用）
go test -race ./internal/...
```

**耗时测试文件**（仅在需要时运行）:
- `tests/integration/*_duplicate_billing_*` - 重复计费保护测试
- `tests/integration/*_streaming_*` - 流式处理测试

## 设计模式

- **状态机模式**: 请求生命周期管理
- **适配器模式**: 多数据库支持
- **工厂模式**: 请求处理器创建
- **断路器模式**: 健康检查与故障转移

## 调试技巧

- **请求追踪**: 使用 `req-xxxxxxxx` 格式的请求 ID 过滤日志
- **端点状态**: 应用内「端点管理」页面查看健康状态
- **性能监控**: 应用内「概览」页面查看热池统计

## 版本历史

| 版本 | 日期 | 主要更新 |
|------|------|----------|
| v5.0.0 | 2025-12-08 | Wails 桌面应用重构，移除服务器版本 |
| v4.1.0 | 2025-12-04 | 内存热池架构，80% 数据库写入优化 |
| v4.0.0 | 2025-12-03 | 配置简化，移除"组"概念 |
| v3.5.0 | 2025-10-12 | 状态机重构，MySQL 支持 |

详细更新记录请参阅 `CHANGELOG.md`。
