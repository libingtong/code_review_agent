# AGENTS.md

> 给在这个仓库里工作的 agent 的索引。先读这一页，再按需打开 `docs/` 里的专题文档。
> 面向用户的使用说明在 [README.md](README.md)，运行步骤在 [RUNNING.md](RUNNING.md)。

## 这是什么

一个用 Go 写的多 agent 代码审计程序：TUI 前端 + 多阶段 agent 团队（侦察 → 审计 + 论坛管理员），
模型调用走"渠道池"（GLM / MiniMax 等），带 5 小时订阅余量调度、自适应并发、工作时段门控和任务队列。

- 入口：[main.go](main.go)（`-config`、`-dir`、`-check`）
- 无第三方业务依赖，TUI 用 bubbletea；配置用 `gopkg.in/yaml.v3`

## 一分钟上手

```bash
export GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOTMPDIR=$PWD/.gotmp   # 沙箱/无 $HOME 写入时必需
go build ./... && go vet ./... && go test ./...
./code-review-agent -config config.yaml -check    # 非交互自检：渠道/余量/模型/时段/队列
./code-review-agent -config config.yaml           # 进 TUI
```

## 仓库地图

| 路径 | 职责 | 关键文件 |
| --- | --- | --- |
| [internal/config](internal/config) | YAML 解析、默认值、校验；渠道/配额/时段模型；套餐预设 | [config.go](internal/config/config.go)、[providers.go](internal/config/providers.go)、[plans.go](internal/config/plans.go) |
| [internal/provider](internal/provider) | 渠道池、5H 余量窗口、探针、路由与故障切换、AIMD 并发、限额持久化 | [provider.go](internal/provider/provider.go)、[router.go](internal/provider/router.go)、[quota.go](internal/provider/quota.go)、[limiter.go](internal/provider/limiter.go)、[probe.go](internal/provider/probe.go) |
| [internal/schedule](internal/schedule) | 工作时间段（全局 + 渠道覆盖）、门控判定、策略持久化 | [window.go](internal/schedule/window.go)、[plan.go](internal/schedule/plan.go)、[gate.go](internal/schedule/gate.go)、[store.go](internal/schedule/store.go) |
| [internal/orchestrator](internal/orchestrator) | 任务队列：导入、串行执行、断点续跑、产出归档 | [queue.go](internal/orchestrator/queue.go)、[runner.go](internal/orchestrator/runner.go) |
| [internal/agent](internal/agent) | agent 团队与单体：回合循环、阶段屏障、论坛/IRC、上下文压缩、预算、会话 | [team.go](internal/agent/team.go)、[agent.go](internal/agent/agent.go)、[budget.go](internal/agent/budget.go)、[retry.go](internal/agent/retry.go)、[session.go](internal/agent/session.go) |
| [internal/llm](internal/llm) | OpenAI 兼容客户端（chat / responses 双协议）、SSE 流、原生工具、用量上报 | [openai.go](internal/llm/openai.go)、[native_tools.go](internal/llm/native_tools.go)、[usage.go](internal/llm/usage.go) |
| [internal/tools](internal/tools) | 只读代码审计工具：文件、搜索、Git、review 状态、todo/finding | [registry.go](internal/tools/registry.go)、[files.go](internal/tools/files.go)、[git.go](internal/tools/git.go)、[review.go](internal/tools/review.go) |
| [internal/forum](internal/forum) | agent 之间的地下论坛：帖子、通知、共识投票、版务 | [forum.go](internal/forum/forum.go)、[posts.go](internal/forum/posts.go)、[consensus.go](internal/forum/consensus.go) |
| [internal/tui](internal/tui) | bubbletea 界面：状态/论坛双栏、命令、弹窗（预算/源/时段/任务/会话） | [model.go](internal/tui/model.go)、[commands.go](internal/tui/commands.go)、[view.go](internal/tui/view.go)、[providers.go](internal/tui/providers.go)、[tasks.go](internal/tui/tasks.go)、[window_form.go](internal/tui/window_form.go) |
| [internal/report](internal/report) | 审计报告构建（JSON/Markdown），TUI 与编排器共用 | [report.go](internal/report/report.go) |
| [internal/prompt](internal/prompt)、[prompts/](prompts)、[skills/](skills) | 系统提示词、模板渲染、skill 加载 | [prompt.go](internal/prompt/prompt.go) |
| [docs/](docs) | 专题文档（架构、渠道、时段、编排、配置、开发、排障） | 见下方索引 |

## 专题文档索引

| 文档 | 什么时候读 |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | 想搞清一次审计从 TUI 到模型调用的完整链路、阶段屏障、会话/状态落盘 |
| [docs/providers.md](docs/providers.md) | 改渠道、余量探测、故障切换、429 并发、自设上限 |
| [docs/schedule.md](docs/schedule.md) | 改工作时段、跨午夜/周几、渠道覆盖、门控判定顺序 |
| [docs/orchestration.md](docs/orchestration.md) | 改任务队列、断点续跑、批量审计产出 |
| [docs/configuration.md](docs/configuration.md) | 改配置字段/默认值/校验，或排查"配置加载失败" |
| [docs/development.md](docs/development.md) | 加功能、加测试、加渠道厂商、提交与分支规范 |
| [docs/troubleshooting.md](docs/troubleshooting.md) | 用户报"找不到模型 / 余量不足 / 不进任务"等具体症状 |
| [docs/plan-multi-provider.md](docs/plan-multi-provider.md) | 多源调度这一轮的历史设计与实施记录 |

## 必须遵守的约定

1. **分支**：`main` 是唯一开发分支，`master` 只是它的快进镜像。同步用 `./scripts/sync-master.sh`（不在 `main` 上或工作区不干净会直接失败）。
2. **提交规范**：一次提交说清"为什么"，中文 commit message 可以；不要提交生成的审计报告/结果 JSON（`.gitignore` 已覆盖，`state/`、`reports/`、`*审计报告*.md` 都不入库）。
3. **密钥**：只走 `api_key_env` + `.env`（`.env` 已忽略）。不要把 key 写进 `config.yaml` 或代码。
4. **模型协议**：agent 只走**原生 function calling**，一回合一个工具调用；不要引入文本工具协议回退（见 [agent.go](internal/agent/agent.go) 的 `chatStreamOnce`/`validateNativeResponse`）。
5. **并发安全**：池内 `p.mu` 下不要再次 `RLock`（曾因此死锁）；持锁路径用 `clockOpenAt` 这类不取锁的辅助函数。
6. **用量记账**：一次逻辑请求的 token 先在 `attemptMeter` 里累积，**只有成功才提交**；失败请求不得计入额度（否则会凭空开一个 5 小时窗口）。
7. **事件与 UI**：后台状态通过 `Team.publish(Event)` 上报，UI 只读快照（`Snapshot()`/`Statuses()`），不要把可变结构交给 UI。
8. **并发学习**：稳定并发写入 `state/concurrency.json`，启动时用学习值起步；`pin: true` 表示只降不升。改动限流器时必须保留"变化即重置 stable"的语义，否则会把抖动值当成稳定值记下来。

## 常见改动落点

| 想改 | 去哪里 |
| --- | --- |
| 加一个模型厂商/套餐档位 | [internal/config/plans.go](internal/config/plans.go) 加 preset；必要时在 `applyVendorDefaults` 补端点/模型 |
| 改余量探测字段 | 配置里的 `quota.probe.bind`（支持多候选路径与 `=100` 字面量），代码在 [probe.go](internal/provider/probe.go) |
| 改换源/降并发策略 | [internal/provider/router.go](internal/provider/router.go)、[limiter.go](internal/provider/limiter.go) |
| 改暂停/恢复文案与语义 | [internal/agent/retry.go](internal/agent/retry.go)、[internal/schedule/gate.go](internal/schedule/gate.go) |
| 加一个 TUI 命令 | [internal/tui/commands.go](internal/tui/commands.go) 的 `submit` + 对应面板文件 |
| 改审计报告格式 | [internal/report/report.go](internal/report/report.go)（TUI `/export` 与编排器产出共用） |

## 排障入口（详见 [docs/troubleshooting.md](docs/troubleshooting.md)）

| 症状 | 先看 |
| --- | --- |
| `找不到模型` / 模型名报错 | `-check` 会向端点请求 `/models` 并列出可用名字 |
| `所有源余量均不足` | `-check` 的探针行 + `/providers`；注意"时段外/冷却/自设上限"三种刹车都会被写进原因 |
| 频繁 429 | 并发自适应在降档，`/providers` 看 `并发 x/y · 429 次数` |
| `mmx: executable file not found` | MiniMax 余量探针依赖官方 CLI；不装则退化为本地计量 |
| 配置加载失败 | [docs/configuration.md](docs/configuration.md) 的校验规则表 |
