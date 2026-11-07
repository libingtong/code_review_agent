# 开发计划：多 LLM 源管理 + 5H 余量调度 + 任务编排

> ## 实施状态（M0–M7 已完成，分支 `feat/multi-provider-orchestrator`）
>
> | 里程碑 | 状态 | 落地位置 |
> | --- | --- | --- |
> | M0 前置修复 | 完成 | `config.MaxTeamAgents`、`internal/report`、`Team.Workspace/SessionPath` |
> | M1 源池骨架 | 完成 | `internal/provider/{provider,classify}.go`、`config.Providers` |
> | M2 5H 余量 | 完成 | `internal/provider/{quota,quota_manager,probe}.go` |
> | M3 路由切换 | 完成 | `internal/provider/router.go`、`agent/retry.go`、`Team.PauseInfo` |
> | M4 时间窗 | 完成 | `internal/schedule/{window,gate}.go` |
> | M5 任务编排 | 完成 | `internal/orchestrator/{queue,runner}.go` |
> | M6 TUI | 完成 | `tui/{providers,tasks,window_form}.go`、`/providers /quota /window /tasks` |
> | M7 文档与回归 | 完成 | `config.multi.yaml`、README/RUNNING、`-check`、`go test -race` 全绿 |
>
> 实施期相对原计划的三处调整（都是为了少改老代码）：
> 1. 跨源续接失效放在**池内**（`affinityContext` 按 response id 归属丢弃 `previous_response_id`），不再改 `agent/context.go`；Agent 完全无感。
> 2. 压缩走**每源辅助通道**（`Provider.aux`：同源 endpoint/key + `compress_openai` 的模型与输出上限），既保住"压缩用便宜模型"，又让压缩也享受换源。
> 3. 手动审计同样受时间窗约束，`go` 是显式豁免；编排器用 `waitForGate` 自动等待。
>
> 追加需求（回合 4）：**频繁 429 → 自动调整并发**。已实现 `internal/provider/limiter.go`
> （按源 AIMD 限流器）：429 减半、连续成功 +1、并发触底仍 429 才判定为额度问题；
> 限的是在飞请求而非 Agent 进程；变化写入事件流与 `/providers`、`-check`。
> 配置段 `concurrency:`，默认上限 = 团队规模、起步 4、下限 1。
>
> 追加需求（回合 5）：
> 1. **工作时间段可自定义**：`/window` 改为多窗口编辑器（增删改、按周几、空格开关），
>    并把结果持久化到 `schedule.store_file`（默认 `state/schedule.json`），启动时优先于
>    config.yaml；命令行式 `/window 22:00-06:00 mon-fri`、`/window off|on` 同样可用。
>    删除最后一个窗口会自动关闭限制，避免"永远暂停"。
> 2. **5H 余量可自定义**：`quota.max_usage_ratio`（如 0.9 只用自己的 90%）与
>    `quota.reserve_units`（按绝对值保留），运行时 `/limit <ID> 90%|reserve N|off`，
>    持久化到 `state/limits.json`。到上限时该源标记"已达自设上限"并换源；两源都到上限时
>    暂停原因明确区分于厂商额度耗尽。
>
> 追加需求（回合 7）：**quota 跟渠道、按套餐区分容量**。新增 `providers[].vendor`
> 与 `providers[].plan`（预设 `glm-lite/glm-pro/glm-max/minimax-mplan`），容量、探测端点、
> 抵扣系数、计价高峰都由预设填充且显式值可覆盖；同厂商多渠道各自独立计量、独立上限。
> 按实测 GLM 响应修正绑定（`TOKENS_LIMIT` 只有 `percentage`，档位在 `data.level`）：
> 新增 bind 字面量 `=100`、`probe.unit: percent` 折算、`data.level` 档位核对。
>
> 追加需求（回合 8）：
> 1. **兜底计量改按 GLM-5.3-Flash**：`credits_per_million_tokens` 预设与兜底默认值从 1545 改为
>    515（2.3 输入 / 0.56 缓存 / 8 输出 每万 token 折算），避免按 5.3 的高价把消耗高估约 3 倍。
> 2. **工作时间段支持全局 + 渠道局部**：新增 `providers[].schedule` 与 `schedule.Plan`
>    （全局 policy + 每渠道 override），`enabled: false` 表示该渠道忽略全局、全天可用；
>    路由只挑当前开窗的渠道，`Gate` 只要还有任一渠道开窗就不整体暂停；
>    `/window <渠道> always|inherit|HH:MM-HH:MM [days]` 可运行时修改并持久化。
>
> 验证命令：
> ```bash
> go build ./... && go vet ./...
> go test ./... -race
> ./code-review-agent -config config.multi.yaml -check
> ```

> 目标程序：`code_review_agent`（Go 1.20 module，BubbleTea TUI，无三方依赖偏好）
> 基线状态：`go build ./...` / `go test ./...` 全绿（本地 go1.27.3 darwin/arm64 验证）
> 本文档只描述"怎么做"，不含实现代码。

---

## 0. 需求 → 能力分解

| 你提的需求 | 拆出来的能力 | 归属 |
| --- | --- | --- |
| 我有 GLM / MiniMax 两个订阅，想用空闲 token 跑审计 | 多源注册、按源独立的模型参数、按源独立计费口径 | `internal/provider` |
| 查询 5H 订阅余量 | 5 小时窗口状态机 + 余量探针（HTTP / CLI / 本地计量）+ 窗口持久化 | `internal/provider/quota` |
| 余量不足切换到另一个 | 路由策略（粘性 + 滞回）、故障切换、限流/欠费错误分类 | `internal/provider` + `agent/retry.go` |
| 都不足时暂停，有余量自动启动 | 调度闸门：`running / paused_quota / paused_window / paused_all`，按 resetAt 定时唤醒 | `internal/schedule` |
| 设置工作时间段（21:00–08:00） | 跨午夜时间窗、时区、按周几、DST 安全 | `internal/schedule` |
| 批量导入任务，自动依次完成 | 任务队列（导入/持久化/断点续跑/单并发执行/产出归档） | `internal/orchestrator` |
| TUI 里能看见、能改 | `/providers` `/quota` `/window` `/tasks` + 顶栏状态条 | `internal/tui` |

---

## 1. 现状勘察（关键事实，实现必须尊重）

### 1.1 模型调用链

- `internal/llm/openai.go:111` `Client{Chat, ChatStream}`；`openai.go:59` `ToolClient{ChatTools}`。
  两者是**唯一**的模型出入口，一个实现了这两个接口的"路由客户端"可以完全透明替换。
- `internal/agent/agent.go:825` `chatStreamOnce` 对 `a.client` 做 `llm.ToolClient` 断言，**失败即报错不降级**（原生工具协议是硬约束）。
- `internal/llm/usage.go:27` `WithUsageObserver(ctx, fn)` → 每次请求的 `UsageUpdate{RequestID, TotalTokens, Estimated, Final}`。
  `Team` 在 `agent/budget.go:110` 已挂载观察者做 token 记账。**这是现成的按源计量钩子**。
- `internal/llm/openai.go:66` `ResponsesContinuation`（`previous_response_id` 续接）+ `internal/agent/context.go:19–42` 的 `responseID/responseMessageIndex` 失效与提交。
  ⚠️ **续接 ID 是"源私有"的**：换源后继续带旧 `previous_response_id` 必然 404/400。切源必须让续接点失效并重发完整历史。

### 1.2 失败与停止语义

- `internal/agent/retry.go:44` `isBillingExhausted` 命中关键词列表 → `retry.go:83` 立刻 `onDisconnect` → `retry.go:120` `modelDisconnected` 取消**整个团队**（含 moderator）。
  这就是"余额不足立即停"的现状，也是本次要改成"先换源、全空才暂停"的唯一入口。
- `internal/agent/retry.go:28` `isModelDisconnect` 网络类错误无限重试（1s→5min 封顶）。
  429 目前被当成网络错误无限重试——**这是 bug 级缺口**：欠额/限流应该换源而不是干等。
- `internal/agent/team.go:399` `Run` 单飞（`running` 守卫），`team.go:157` `SetWorkspace` 要求非运行态。

### 1.3 会话与产出

- `internal/agent/session.go:59` `SaveSession` / `session.go:111` `LoadSession`（Version 2）。
  ⚠️ `session.go:145` 把 `ReconAgents/AuditAgents` 卡在 **1..32**，而 `config.go:139` 允许到 **666**。任务编排要跨进程续跑就必须先修这个上限不一致（见 M0）。
- 报告导出目前写在 TUI 里（`internal/tui/commands.go:478 exportReport`），编排器拿不到 → 需要抽包（见 M0）。

### 1.4 TUI 形态

- 单 `Model`，`internal/tui/model.go:468 runQuery` 起一次 `Team.Run`，`syncTeam()` 轮询快照。
- 弹窗统一走 `modalState`（`internal/tui/modal.go:17`），表单范式已存在于 `internal/tui/budget.go`（Tab 切字段 / Ctrl+S 保存 / Esc 取消）——新表单照抄这套。
- 命令分发在 `internal/tui/commands.go:21 submit` 的 switch，加命令就是加 case。

### 1.5 配置

- `internal/config/config.go:95 Load` 逐项校验并 `applyDefaults`；`CompressionThreshold()`（`config.go:165`）用 `cfg.OpenAI` 的 context/output 算压缩阈值 —— 多源后必须改成"所有启用源的最小值"，否则会按最宽松的源算，压缩不及时直接超限。

---

## 2. 目标架构

```
main.go
 ├── config.Load           providers[] / quota / schedule / orchestrator
 ├── provider.NewPool      每个源持有自己的 *llm.OpenAIClient（独立 cfg）
 │     ├── QuotaManager    探针轮询 + 窗口状态机 + 落盘
 │     └── Router          选源/换源/粘性/滞回
 ├── schedule.Gate         时间窗 + 余量闸门 → running/paused_*
 ├── agent.NewTeam(..., pool, pool, registry)   ← pool 同时充当 client 与 compressClient
 └── orchestrator.Runner   任务队列 → SetWorkspace → LoadSession? → Run → SaveSession → 报告
      └── tui              /providers /quota /window /tasks
```

三个新包 + 五处老代码改动，其余零打扰：

| 包 | 职责 | 是否触碰网络 |
| --- | --- | --- |
| `internal/provider` | 源注册、池、路由、失败分类、计费口径 | 是（探针） |
| `internal/schedule` | 工作时间窗 + 闸门状态机 | 否 |
| `internal/orchestrator` | 任务队列与串行执行 | 否 |
| `internal/report` | 从 tui 抽出的报告构建（json/md） | 否 |

---

## 3. 关键设计

### 3.1 源配置（每源一份完整 `OpenAIConfig`）

沿用现有 schema，天然支持每源不同的 `thinking/reasoning_effort/max_context_tokens/api_interface`：

```yaml
providers:
  - id: glm
    label: "GLM Coding Plan"
    enabled: true
    weight: 1                     # 同余量时的优先权重
    openai:
      api_interface: chat         # GLM 官方给的两种协议见下
      base_url: https://open.bigmodel.cn/api/coding/paas/v4   # OpenAI Chat
      # base_url: https://open.bigmodel.cn/api/v1             # OpenAI Responses
      # base_url: https://open.bigmodel.cn/api/anthropic      # Anthropic（本程序不用）
      api_key: ""
      api_key_env: GLM_API_KEY
      model: glm-5.3
      thinking: {type: enabled}
      reasoning_effort: high
      temperature: 1
      top_p: 1
      max_context_tokens: 200000
      max_output_tokens: 65536
      timeout_seconds: 300
      stream: true
    quota:
      window: 5h
      probe:
        type: http
        interval_seconds: 120
        url: https://open.bigmodel.cn/api/monitor/usage/quota/limit
        headers:
          Authorization: "Bearer ${api_key}"
        # 版本容错：多组候选路径，命中第一个非空即可
        bind:
          used:  ["data.limits[type=TOKENS_LIMIT].usage", "data.used"]
          total: ["data.limits[type=TOKENS_LIMIT].limit", "data.total"]
          reset: ["data.limits[type=TOKENS_LIMIT].nextResetTime", "data.reset_time"]
      # 本地计量兜底：GLM-5.3 官方抵扣系数（积分/万 token）
      credit_rates:
        glm-5.3:       {input: 6.9, cached: 1.7, output: 24}
        glm-5.3-flash: {input: 2.3, cached: 0.56, output: 8}
      offpeak: {factor: 0.5, peak: "Mon-Fri 14:00-18:00"}   # 非高峰 5 折
      total_credits: 12000        # 套餐档位（Pro）；为 0 时只用探针

  - id: minimax
    label: "MiniMax M Plan"
    enabled: true
    openai:
      api_interface: chat
      base_url: https://api.minimax.cn/v1
      api_key_env: MINIMAX_API_KEY
      model: MiniMax-M3.1-Flash
      thinking: {type: adaptive}
      reasoning_effort: max
      max_context_tokens: 1000000
      max_output_tokens: 65536
      timeout_seconds: 120
      stream: true
    quota:
      window: 5h
      probe:
        type: command            # 官方 CLI 有 quota 子命令，最稳
        command: ["mmx", "quota", "--output", "json"]
        timeout_seconds: 20
        bind:
          used:  ["data.used_percent", "usage.used"]
          total: ["data.total_percent", "usage.total"]
          reset: ["data.reset_at", "usage.window_end"]
      total_credits: 0
```

**多源兼容旧配置**：`providers` 为空时，用现有 `openai:` 段自动合成单源 `legacy`，行为与今天完全一致（零破坏）。

### 3.2 5H 余量：三层探针 + 一套窗口状态机

**为什么三层**：GLM 与 MiniMax 的 5 小时额度目前**都没有公开文档化的查询 API**（官方文档只给控制台页面：GLM「用量统计」、MiniMax `mmx quota`），且字段随时会变。硬编码单一接口等于把程序焊死。所以：

1. `http` 探针：直接打控制台/网关接口（上面的默认值按社区通行端点填，可通过 `url`+`bind` 现场改，无需重编译）。
2. `command` 探针：跑官方 CLI（MiniMax 的 `mmx quota` 就是文档化能力），解析 stdout JSON 或正则。
3. `local` 计量：用 `WithUsageObserver` 的实测 token × `credit_rates` 推算积分消耗，与 `total_credits` 比对；非高峰 5 折按官方规则折算。
4. 被动信号（永远开启，优先级最高）：任何一次真实请求返回 `429 / insufficient / quota exceeded / 额度` → 直接把该源置为 `exhausted` 并进入 `cooldown`，`retry_after`/错误文本里的时间戳解析为 `resetAt`。

**失败策略是 fail-open**：探针报错/字段缺失 → 不阻断作业，降级到本地计量；本地计量也不知道总量（`total_credits: 0`）→ 视为"未知但可用"，直到被动信号触发。**探针永远不因为自己挂了而停掉审计**。

**窗口状态机**（两家规则一致：首次使用开始计时、5 小时后刷新、未用完不累积）：

```
状态: {windowStart, used, total, resetAt, source: probe|local|passive, confidence}
tick():
  if now >= resetAt:          used = 0; windowStart = 0; resetAt = 0; exhausted = false
  if 请求成功:                 if windowStart == 0 { windowStart = now; resetAt = now+5h }; used += credits(delta)
  if probe 有值:              以 probe 为准覆盖 used/total/resetAt，confidence=high
  remaining = total - used;   usable = !exhausted && now >= cooldownUntil && (total==0 || remaining >= min_headroom)
```

落盘 `state/quota.json`（原子写 + 版本号），进程重启后窗口不重置——不然一重启就误判"满额可用"，5 小时窗口会被打穿。

### 3.3 路由：粘性 + 滞回（保护缓存命中率）

README 明确"缓存 97%–99%"是核心卖点。**切源会清空上游 prompt cache**，所以策略必须保守：

```
acquire():
  1. 当前 active 仍 usable 且 remaining_ratio >= switch_below (默认 0.15) 且 dwell >= min_dwell(300s) → 保持
  2. 否则在可用源里选 remaining 最大者（同余量比按 weight）
  3. 无可用源 → 返回 ErrAllExhausted，交给 Gate 暂停
  4. 每次真实换源 → 记 event（在 TUI 事件流里可见："glm 余量 12% → 切到 minimax"）
```

参数全部可配：`routing: {switch_below: 0.15, min_dwell_seconds: 300, prefer: sticky|highest}`。

### 3.4 故障切换落点（改 3 个函数，不动协议层）

| 位置 | 现状 | 改为 |
| --- | --- | --- |
| `retry.go:44 isBillingExhausted` | 命中即停全队 | 拆成 `classifyModelError(err) → {billing, rate_limited, auth, network, context, protocol}` |
| `retry.go:83` | 调 `onDisconnect` 取消团队 | `pool.ReportFailure(active, kind, err)`；`pool.Failover()` 成功 → 同参数重试本次请求（不计重试次数）；失败 → `Team.pauseAll("quota_exhausted")` |
| `retry.go:28 isModelDisconnect` | 429 当网络错误死等 | 429/429-like 走 `rate_limited` 分支：设该源 `cooldownUntil`，立刻换源，不睡 5 分钟 |
| `agent/context.go:33 responsesRequestContext` | 无源意识 | 记录 `a.responseProvider`；`pool.ActiveID() != a.responseProvider` → 不带 `previous_response_id`，改为发完整历史（一次性的成本，可接受） |

`Team` 新增 `PauseAll(reason)`：取消 runCtx 但**保留全部进度**，`budget.reason` 写"全源余量耗尽，等待 X 恢复"，TUI 状态条显示倒计时。

### 3.5 工作时间窗 + 自动启停（`internal/schedule`）

```yaml
schedule:
  enabled: true
  timezone: Asia/Shanghai
  windows:
    - {start: "21:00", end: "08:00", days: [mon,tue,wed,thu,fri,sat,sun]}
    - {start: "12:00", end: "13:30", days: [sat,sun]}
  on_window_close: pause        # pause | finish_current_task
  poll_seconds: 30
```

- 跨午夜（21:00→08:00）用"区间跨越 date line"判定，不用简单 `start<now<end`。
- `time.LoadLocation` 拿到 IANA 时区，DST 当天按真实时刻判定（本地时间不存在/重复的时刻按 Go 的规则取最近有效点）。
- 闸门状态机：

```
running ──(窗口关闭)──▶ paused_window ──(下个窗口开始)──▶ running
   │                                              ▲
   ├──(all providers exhausted)──▶ paused_quota ───┘ (最早 resetAt + 60s 抖动)
   └──(用户 Esc / 任务全部完成)──▶ stopped
```

- 唤醒动作 = 恢复上次会话：`LoadSession(sessionPath)` → `Run("继续基于当前团队状态和论坛审计，不要从头开始")`（就是 TUI `go` 走的同一条路，已有实现）。
- 边界：`finish_current_task` 模式下窗口关闭不打断，只是不再启动下一个任务。

### 3.6 任务编排（`internal/orchestrator`）

```yaml
# tasks.yaml —— 三种导入形式
tasks:
  - id: T001
    title: "智通人才-就在香城"
    dir: /Volumes/Disk/Code/code_review_tools/job5156
    prompt: "请对当前工作区进行安全审计……"
    budget: {hours: 3, minutes: 0, tokens: 0}
    agents: {recon: 4, audit: 12}
    retries: 1
```

也支持：`tasks.txt` 每行 `目录 | 提示词`；或 `/tasks import <父目录> --each-subdir` 把每个子目录当一个任务。

执行循环（严格串行，因为 `Team.Run` 是单飞的）：

```
for task in queue.pending():
    gate.Wait()                    # 时间窗 + 余量
    runner.SetWorkspace(task.Dir)
    runner.ConfigureBudget(...)    # 每任务预算
    runner.Run(ctx, task.Prompt)   # 中途暂停 → SaveSession → 状态 paused，任务不丢
    session := SaveSession(sessions/<task.id>.json)
    report.Export(json+md → reports/<task.id>/)
    task.Status = done | failed(retries-- → 重排队尾)
```

持久化：`orchestrator/state.json`（原子写，含当前任务 + 会话路径 + 队列游标）→ 进程被杀也能接着跑。
产出隔离：每任务独立 `reports/<id>/report.json`、`report.md`、`session.json`，互不覆盖。
并发：`concurrency: 1`（Team 单例约束）；保留字段，未来多 Team 时放开。

### 3.7 TUI

- 顶栏状态条（`view.go:130 summary` 扩展）：
  `GLM 5h 62% ⟶18:42 ▸ ACTIVE | MM 5h 88% ⟶20:10 | 窗口 21:00-08:00 ●开启 | 任务 2/7 | 预算 1h12m`
- 新命令：
  - `/providers` 源列表（余量、窗口、状态、当前 active）→ ↑↓ 选、`e` 启停、`1..9` 强制切换、`r` 立即刷新
  - `/quota [id]` 强刷探针并显示**原始响应**（字段对不上时用户能自己 `bind` 修配置，不靠改代码）
  - `/window` 工作时间窗表单（照抄 `budget.go` 的 Tab/Ctrl+S 范式）
  - `/tasks` 队列视图：导入、排序、跳过、重试、看当前 task 进度
- 事件流复用 `applyEvent`：切源/暂停/恢复都作为 `Event{Kind:"info"}` 落到已运行事件列表，不新增渲染路径。

---

## 4. 里程碑（按可验收粒度切，建议顺序执行）

### M0 · 前置修复（半天，先做，否则后面全是坑）
1. `session.go:145` 团队规模上限 32 → 与 `config.go:139` 的 666 对齐（或统一抽 `config.MaxAgents`）。
2. 把 `tui/commands.go:478 exportReport` 的报告构建抽到 `internal/report`，TUI 与编排器共用。
3. 给 `Team` 增加只读 `SessionPath()`，让编排器知道该存哪。
- **验收**：`go test ./...` 全绿；`/export` 行为不变（对照现有 `report.json` 字段逐一比对）。

### M1 · 源池骨架（1 天）
`internal/provider/provider.go`、`pool.go`；`config.go` 加 `Providers []ProviderConfig` + 校验 + 默认值 + legacy 合成；`main.go` 注入 pool。
- **验收**：`providers` 只配一个源时，行为与今天完全一致；`pool` 实现 `llm.Client` + `llm.ToolClient`；`go vet` 干净；新增 `provider_test.go` 覆盖"legacy 合成/配置校验"。

### M2 · 5H 余量（1.5 天）
`quota.go`（窗口状态机）、`probe_http.go`、`probe_command.go`、`probe_local.go`、`window_state.go`（落盘）、`credits.go`（系数/非高峰折算）。
- **验收**（fixture 驱动，不依赖真账号）：`testdata/glm_quota.json`、`mmx_quota.json` 解析出 used/total/reset；窗口到期自动归零；重启后窗口不丢；探针 500/超时 → 降级 local 且不阻断。

### M3 · 路由切换（1.5 天）
`router.go`（粘性+滞回）、`classify.go`（错误分类）、`retry.go` 三处改动、`context.go` 源亲和、`Team.PauseAll`。
- **验收**：fake client 注入 `insufficient` → 自动换源、**不**取消团队；两源都 insuffice → `paused_quota` 且进度完整；429 不再走 5 分钟死等；换源后首个请求不带 `previous_response_id`（单测断言）。

### M4 · 时间窗 + 闸门（1 天）
`internal/schedule/window.go`、`gate.go`；Team/编排器接入；`/window` 表单。
- **验收**：跨午夜、周末窗、DST 各 3 个用例；窗口关闭 → `paused_window` 且自动保存；到点自动 `LoadSession`+`Run` 恢复（用 fake clock 驱动，不真的等 8 小时）。

### M5 · 任务编排（1.5 天）
`internal/orchestrator/{task,queue,runner}.go`；三种导入；状态持久化；每任务产出目录。
- **验收**：3 任务队列在"中途暂停+进程重启"后从断点继续；失败重试落队尾；`/tasks` 能看进度；产出不互相覆盖。

### M6 · TUI 收口（1.5 天）
`tui/providers.go`、`tui/tasks.go`、`tui/window_form.go`、`view.go` 状态条、`commands.go` 命令注册、`/help` 更新。
- **验收**：120×40 与窄屏（<100 列）两种布局下不串行；`view_test.go` 补快照断言。

### M7 · 联调与文档（1 天）
真账号 smoke（各源 1 次 1 token 请求 + 1 次探针）、`README` 新章节、`RUNNING.md`、示例配置 `config.multi.yaml`、`config.smoke.yaml` 更新。
- **验收**：`go test ./... -race` 全绿；连续 3 个任务跑通；手工拔一个源的 key（模拟欠费）验证换源与恢复。

合计约 **8 个工作日**，M0–M3 完成即可日常使用（双源+自动切换），M4–M6 才是"无人值守"。

---

## 5. 测试矩阵

| 层 | 用例 | 手段 |
| --- | --- | --- |
| 解析 | 两个源的余量 JSON 字段漂移（多候选 bind 路径） | `testdata/*.json` 表驱动 |
| 状态机 | 5h 窗口刷新、未用不累积、重启不丢、被动信号置 exhausted | fake clock |
| 路由 | 粘性保持、滞回阈值、全空 ErrAllExhausted、dwell 未到时禁止抖动 | 表驱动 |
| 错误 | `insufficient_quota`/`429 + retry-after`/`401`/网络断/上下文超限 各自分类 | 构造 error 文本 |
| 续接 | 换源后 `previous_response_id` 被丢弃 | 拦截 request body 断言 |
| 时间窗 | 跨午夜、跨周、DST、`finish_current_task` | `time.LoadLocation` + 假 now |
| 编排 | 断点续跑、重试、产出隔离、并发=1 | 临时目录 + fake Team |
| 回归 | 现有 21 个测试包不退化 | `go test ./... -race` |

---

## 6. 风险与对策

| 风险 | 影响 | 对策 |
| --- | --- | --- |
| GLM / MiniMax 的余量接口**未文档化**，字段可能变 | 余量显示为空、切换判据失真 | 三层探针 + `bind` 多候选路径 + `/quota` 打原始响应 + 本地计量兜底；探针 fail-open 永不阻断 |
| GLM Coding Plan 条款限定"仅官方指定工具"使用，非白名单工具可能被限流/风控 | 该源随时可能不可用 | 程序不写死任何源：把 GLM 当普通 OpenAI 兼容源，用 `weight` 降权、`min_headroom` 提前让位给 MiniMax；也可随时换成标准 API key（改 `base_url`/`api_key_env` 即可，不动代码） |
| 切源清空上游 prompt cache（README 主打 97–99% 命中） | 成本上升 | 粘性 + 滞回 + `min_dwell`；只在余量真的见底时切 |
| 跨源续接失效 → 需要重发完整历史 | 单次请求 token 暴涨 | 切换点前触发一次 `compressContext`（现成能力，`agent.go:929`），把历史压到阈值内再换源 |
| 各源上下文/输出上限不同（1M vs 200K） | 压缩阈值算错 → 直接 400 | `CompressionThreshold()` 改为取**启用源最小值**；每源独立校验 `output < context` |
| 暂停/恢复打断 worker 导致状态半提交 | 进度回退 | 复用现有 checkpoint（`team.go:258 capture`）：暂停前 `SaveSession`，恢复走 `LoadSession`+`go` 同路径 |
| 探针请求本身触发限流 | 被平台额外计费/风控 | 探针间隔 ≥60s + 抖动，探针 429 指数退避，探针不计入审计用量 |

---

## 7. 明确不做（本期）

- 不做多 Team 并发（`Team.Run` 单飞约束，收益低风险高）。
- 不做 Web UI。
- 不做跨源"能力路由"（如"简单任务给 Flash、难任务给 Pro"）——先把余量跑通，能力路由留到 M8 再看。
- 不引入新的第三方依赖：JSON 路径抽取、YAML 解析（已有 `yaml.v3`）、HTTP 都用标准库手写，保住"编译即可用"。

---

## 8. 配置迁移

- 旧 `config.yaml` 原样可用（`providers` 缺省 → 自动合成单源）。
- 新模板：`config.multi.yaml`（GLM + MiniMax 双源 + 时间窗 + 任务队列示例），`README` 给最短上手三段：填两个 key env → `/quota` 看余量 → `/tasks import`。
- 敏感信息只走 `api_key_env`，与现有 `.env` 约定一致。
