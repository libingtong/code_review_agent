# 开发与提交

## 构建 / 测试

```bash
export GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOTMPDIR=$PWD/.gotmp   # 无 $HOME 写入权限时必需
go build ./...
go vet ./...
go test ./...                 # 约 10 秒
go test ./... -race           # 改动并发/池子/门控后必须跑
gofmt -l internal main.go     # 应为空
```

单包聚焦：

```bash
go test ./internal/provider/ -run TestFailsOver -v
go test ./internal/schedule/ -run TestPlan -v
```

## 分支与提交

- `main` 是唯一开发分支；`master` 只是快进镜像。
- 同步：`./scripts/sync-master.sh`（不在 `main`、工作区不干净、或两边分叉都会直接失败）。
- 提交信息写"为什么"，中文可以；一个提交一件事。
- 不要提交：`state/`、`reports/`、`sessions/`、`*审计报告*.md`、`*.json`（`internal/**/testdata/**/*.json` 除外）、`.env`。

## 测试约定

| 场景 | 做法 | 例子 |
| --- | --- | --- |
| 渠道行为（换源/429/探针） | `httptest.NewServer` 造假端点 + `chatServer/failingServer` 辅助 | [provider_test.go](../internal/provider/provider_test.go)、[limiter_test.go](../internal/provider/limiter_test.go) |
| 时间相关 | 注入时钟：`pool.now`、`schedule.Gate.Now`、`limiter.now` | [quota_test.go](../internal/provider/quota_test.go) |
| 退避等待 | `pool.SetWaiter(func(ctx, d) error { return ctx.Err() })`，避免真睡 | [limiter_test.go](../internal/provider/limiter_test.go) |
| 时区/DST | 用真实 IANA 时区 + `time.Date(...)` 固定时刻 | [window_test.go](../internal/schedule/window_test.go) |
| 编排 | 假 `Team`（实现 `orchestrator.Team` 接口）+ 临时目录 | [runner_test.go](../internal/orchestrator/runner_test.go) |
| 配置 | `loadBudgetConfig(t, yamlText)` 直接解析字符串 | [providers_test.go](../internal/config/providers_test.go) |
| TUI | 构造 `Model`（`tui.New`）+ `AttachProviders`，直接调面板方法断言 | [sources_test.go](../internal/tui/sources_test.go) |

原则：**测试不依赖真实网络与真实 key**；需要网络的地方一律 httptest；需要时间的地方一律注入。

## 加一个渠道厂商 / 套餐档位

1. 在 [internal/config/plans.go](../internal/config/plans.go) 的 `PlanPresets()` 加 preset（端点、模型、容量、费率、探针）。
2. 若是新厂商，在 `applyVendorDefaults` 补该 vendor 的端点/模型/token 默认值。
3. 若余量接口形态特殊，新增 `probe.unit` 或 `bind` 能力前先看 [probe.go](../internal/provider/probe.go) 现有解析器。
4. 补测试：`plans_test.go`（预设容量/显式覆盖）、`probe_test.go`（响应解析）。

## 加一个 TUI 命令

1. [internal/tui/commands.go](../internal/tui/commands.go) 的 `submit` 里加 `case`，并加入顶部命令白名单。
2. 面板放独立文件（参考 `providers.go` / `tasks.go` / `window_form.go`），状态挂在 `modalState`。
3. 需要异步的（如启动队列）返回 `tea.Cmd`，通过 `eventMailbox` 汇报，不要阻塞 Update。
4. 补 `sources_test.go` 风格的测试；`/help` 文案同步更新。

## 关键不变量（改代码前先读）

1. **原生工具协议**：`chatStreamOnce` 只认 `function_call`；`validateNativeResponse` 拒绝文本协议、缺 `call_id`、未声明工具。一回合的多个调用由 `planBatch` 决定：按顺序执行、去重、只允许一个状态写、独占工具单独成回合，上限 `agent.max_tools_per_turn`。
2. **使用量只在成功时提交**：`runRequest` 里 `commitUsage(err == nil)`；失败请求记账会凭空开窗口（历史 bug）。
3. **窗口滚动只看 `resetAt`**：不要重新引入"必须 start 非零"的条件。
4. **池内锁**：`nextProvider`/`hasReadyProvider`/`Statuses` 持锁，读时钟用 `clockOpenAt`，不要二次 `RLock`。
5. **暂停可恢复**：暂停前 `SaveSession`，`PauseInfo` 记录原因与恢复时刻；`ClearPause` 在下次准入时清空。
6. **UI 只读快照**：`Snapshot()`/`Statuses()` 返回副本；可变结构留在 Team/池内部。
7. **事件有界**：`WorkerStatus.OutputExcerpt`、事件正文都有长度上限，别把整段模型输出塞进事件。

## 调试配方

```bash
# 配置/渠道/余量/模型/时段/队列，全都打印
./code-review-agent -config config.yaml -check

# 只想看探针原始响应（TUI 内）
/quota glm

# 看某个渠道为什么不可用
/providers      # 最近错误、停用原因、时段、冷却、429 次数

# 会话恢复
/session 或 /restore <片段>，然后输入 go
```
