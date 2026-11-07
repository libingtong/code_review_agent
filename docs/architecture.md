# 架构

## 一次审计的完整链路

```
main.go
  config.Load             ← YAML + 默认值 + 校验（渠道/配额/时段/编排）
  prompt.Load             ← prompts/ + skills/
  provider.NewPool        ← 每个渠道一个 *llm.OpenAIClient（含压缩辅助通道）
  pool.LoadQuotaState     ← state/quota.json（窗口消耗）
  pool.LoadLimits         ← state/limits.json（自设上限）
  schedule.FromConfig     ← 全局时段 + 渠道覆盖 → Plan
  tools.NewRegistry       ← 工作区索引（文件清单、review 状态）
  agent.NewTeam           ← 持有 pool / registry / prompts / cfg
  orchestrator.NewRunner  ← 可选：任务队列
  tui.New + AttachProviders(pool, gate, runner)
```

TUI 调 `Team.Run(ctx, input, emit)`；`Team` 是唯一的状态持有者，UI 只读快照。

```
Team.Run
 ├─ 单飞守卫（已在运行则拒绝）
 ├─ 门控准入：gate.Check() → 不在时段/无余量则记 pause 并返回（不启动）
 ├─ startBudgetLocked：时间/token 预算 + llm.WithUsageObserver 记账
 ├─ 阶段循环 recon → audit
 │   ├─ createStageLocked：创建 N 个 Agent（每个 registry.Fork()）
 │   ├─ waitStageIdle：等本阶段所有 worker 完成或被取消
 │   ├─ recon 完成 → buildHandoffLocked → 进入 audit（新建 worker，不继承原始对话）
 │   └─ audit 完成 → reviewStage（论坛共识 / 管理员）
 └─ finishBudget + 状态收尾
```

单个 Agent 的回合（[agent.go](../internal/agent/agent.go)）：

```
for turn:
  prepareRequest   ← 广播/论坛通知注入、上下文压缩检查
  chatStream       ← modelRequest(重试/限流/换源) → chatStreamOnce → pool.ChatTools
  recordResponse   ← 校验原生工具调用（必须有 call_id、只能是本轮声明的工具），并记录全部调用项
  executeNativeTool← 按 planBatch 的结果依次执行工具，每个结果回填为独立消息
  checkpoint       ← 把 worker 快照交给 Team（UI 与阶段屏障只看快照）
```

## 阶段与协作

- **recon**：建模、列文件、建 todo、写项目笔记；结束时必须 `audit_plan_done` 提交结构化交接。
- **audit**：新团队，按需 `read_handoff` 继承地图/笔记/待办（**不继承原始对话**），自行认领文件。
- **forum**：agent 之间用帖子/通知协商分工；`forum_wait` 有界等待；共识工具用于阶段收口。
- **moderator**：独立"论坛管理员"，可读源码、发 IRC、撤销漏洞结论；不参与投票。
- 无限模式（`agent.infinite_mode`）下没有"结束审计"工具，只能被用户停止或预算/余量打断。

## 模型层

`internal/llm` 只认 OpenAI 兼容协议，两种线上协议：

| `api_interface` | 端点 | 用途 |
| --- | --- | --- |
| `chat`（默认） | `{base_url}/chat/completions` | 大多数厂商；GLM Coding Plan 用 `https://open.bigmodel.cn/api/coding/paas/v4` |
| `responses` | `{base_url}/responses` | OpenAI Responses；可带 `previous_response_id` 续接 |

- **原生工具调用是硬约束**：`ChatTools` 收集 `function_call`，文本/XML 一律不算调用（见 `native_tools.go`、`validateNativeResponse`）。
- **续接是渠道私有的**：`ResponsesContinuation` 只对同一渠道有效；跨渠道由池子丢弃并改发完整历史（`affinityContext`）。
- **用量**：`UsageUpdate{RequestID, TotalTokens, Estimated, Final}` 经 `ChainUsageObserver` 同时喂给团队预算与渠道池；池子在请求成功时才把 token 提交进窗口（`attemptMeter`）。

## 持久化

| 文件 | 写入方 | 内容 |
| --- | --- | --- |
| `sessions/<时间戳>.json` | `Team.SaveSession`（每轮结束自动） | teamSession v2：workspace、阶段、每个 worker 的对话/快照/游标、论坛、IRC、预算用量、撤销记录 |
| `sessions/tasks/<任务ID>.json` | 编排器 | 同上，按任务隔离，用于断点续跑 |
| `state/quota.json` | `Pool.SaveQuotaState` | 各渠道 5H 窗口：used/start/resetAt/cooldown/exhausted |
| `state/limits.json` | `Pool.SaveLimits` | `/limit` 设置的自设用量上限 |
| `state/schedule.json` | `schedule.SavePolicy` | 全局时段 + 渠道覆盖（优先于配置文件） |
| `reports/<任务ID>/report.{json,md}` | 编排器 / `/export` | 漏洞、Todo、覆盖、变量、flow、项目笔记 |
| `log_sessions/` | `log_session: true` | 每个 agent 的消息 trace |

所有运行态文件都在 `.gitignore` 里；恢复只用 `sessions/`，`state/` 可删（会重建，但会丢窗口进度）。

## 想改东西时的落点

| 需求 | 位置 |
| --- | --- |
| 新的模型厂商 / 档位 | [internal/config/plans.go](../internal/config/plans.go) |
| 新的审计工具 | [internal/tools/registry.go](../internal/tools/registry.go) + `definitions.go` |
| 新的 UI 面板 | [internal/tui](../internal/tui)，命令注册在 `commands.go` |
| 报告字段 | [internal/report/report.go](../internal/report/report.go) |
| 团队/阶段策略 | [internal/agent/team.go](../internal/agent/team.go)、`moderator.go` |
