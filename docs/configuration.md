# 配置参考

解析与校验：[internal/config/config.go](../internal/config/config.go)（`Load`、`applyDefaults`）、
[providers.go](../internal/config/providers.go)（渠道/配额/时段校验）、[plans.go](../internal/config/plans.go)（套餐预设）。

示例：[config.yaml](../config.yaml)（双渠道）、[config.multi.yaml](../config.multi.yaml)（含时段与编排）、[config.smoke.yaml](../config.smoke.yaml)。

## 顶层结构

```yaml
workspace: .            # 默认审计目录（-dir 可覆盖）

providers: [...]        # 渠道列表，必填（没有 providers 才走单源兼容路径）
quota: {...}            # 状态文件位置（全局，仅此用途）
routing: {...}          # 换源策略
concurrency: {...}      # 自适应并发
schedule: {...}         # 全局工作时间段
orchestrator: {...}     # 任务队列

compress_openai: {...}  # 压缩预算（可省略：取最紧渠道）
prompts: {...}          # 提示词与 skill 目录
agent: {...}            # 团队规模、预算、压缩、会话
```

> 没有 `openai:` 全局段。历史配置若写了 `providers` 也写了 `openai`，`openai` 会被忽略（渠道必须自包含）。

## providers[]

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `id` | 必填 | 唯一，出现在 `-check`、`/providers`、日志 |
| `label` | = id | 展示名 |
| `vendor` | 从 base_url 推断 | `glm` / `minimax` / `openai` / `custom`，决定探针与默认端点 |
| `plan` | 空 | `glm-lite` / `glm-pro` / `glm-max` / `minimax-mplan`，决定容量与费率 |
| `enabled` | true | 停用的渠道不参与路由；无 key 会自动停用并记录原因 |
| `weight` | 1 | 余量相同时的优先级 |
| `openai.*` | 见下 | 该渠道的模型配置（**不继承**任何全局值） |
| `quota.*` | 见下 | 该渠道的窗口/探针/上限 |
| `schedule.*` | 继承全局 | 渠道级时段覆盖 |

`openai` 必填/默认：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `base_url` | vendor/plan 预设 | 例如 GLM `.../api/coding/paas/v4` |
| `model` | vendor/plan 预设 | 名字错会报 `model_not_found`，用 `-check` 查可用值 |
| `api_key` / `api_key_env` | 空 | 推荐 env；两者都空则该渠道会被跳过（除非所有渠道都没 key） |
| `api_interface` | `chat` | `chat` 或 `responses`；两家都支持 responses，GLM 不接受 `previous_response_id`（客户端自动降级） |
| `thinking.type` | `enabled` | `enabled` / `adaptive` |
| `reasoning_effort` | `high` | `low` / `high` / `max` |
| `max_context_tokens` | 预设或必填 | 必须 > `max_output_tokens` |
| `max_output_tokens` | 预设或必填 | 同上 |
| `timeout_seconds` | 120 | 流式请求的空闲超时（见 `streamIdle`） |
| `stream` | false | 建议 true |
| `temperature` / `top_p` | 0 / 1 | |

`quota`：

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `window` | `5h` | 订阅刷新周期 |
| `min_headroom` | 0 | 余量低于该比例就提前让位（按 `ceiling` 计算） |
| `max_usage_ratio` | 1 | 自设用量上限（0.9 = 只用 90%） |
| `reserve_units` | 0 | 按绝对值保留 |
| `probe.type` | 按 vendor | `http` / `command` / `local` |
| `probe.url` / `method` / `headers` | — | `${api_key}` 会被替换 |
| `probe.command` | — | 例如 `["mmx","quota","--output","json"]` |
| `probe.interval_seconds` / `timeout_seconds` | 120 / 15 | 轮询间隔与超时（失败不影响审计） |
| `probe.unit` | 空 | `percent` = 厂商只给百分比，按容量折算 |
| `probe.bind.{used,total,reset,plan}` | 预设 | 多候选路径；`[k=v]` 过滤；`[n]` 下标；`=100` 字面量 |
| `local.unit` | credits | `credits` / `tokens` / `percent` |
| `local.total` | plan 预设 | 窗口容量 |
| `local.credits_per_million_tokens` | 515 | 兜底费率（GLM-5.3-Flash 口径） |
| `local.peak` / `peak_factor` / `offpeak_factor` | 工作日 14–18 点、1 / 0.5 | 非高峰折扣 |

## quota / routing / concurrency

```yaml
quota:
  state_file: state/quota.json     # 5H 窗口消耗
  limits_file: state/limits.json   # /limit 改的上限

routing:
  switch_below: 0.15               # 余量低于该比例才换源
  min_dwell_seconds: 300           # 最短驻留，防抖动
  prefer: sticky                   # sticky | highest
  selection_file: state/selection.json   # /mode 的选择（渠道 + 模型覆盖）

concurrency:
  max_in_flight: 0                 # 0 = recon+audit+1
  initial_in_flight: 4
  min_in_flight: 1
  increase_after_successes: 20
  increase_step: 1
  decrease_factor: 0.5
  increase_cooldown_seconds: 120
  learn: true                      # 稳定后记住并发，下次启动沿用
  pin: false                       # true = 固定在学习值，不再向上试探
  settle_successes: 40             # 连续多少次成功算稳定
  state_file: state/concurrency.json
```

## schedule

```yaml
schedule:
  enabled: false                   # 是否启用全局时段
  store_file: state/schedule.json  # /window 的改动（优先于本文件）
  timezone: Asia/Shanghai
  windows:
    - {start: "21:00", end: "08:00", days: [mon, tue, wed, thu, fri, sat, sun]}
  on_window_close: pause           # pause | finish_current_task
  poll_seconds: 30
```

渠道可用 `providers[].schedule` 覆盖：`enabled: false` = 全天可用。

## orchestrator

```yaml
orchestrator:
  enabled: true
  task_file: tasks.yaml
  state_file: orchestrator/state.json
  report_dir: reports
  session_dir: sessions/tasks
  continue_on_failure: true
  retries: 1
```

## compress_openai / prompts / agent

```yaml
compress_openai:            # 可整段省略
  max_context_tokens: 200000    # 省略则取最紧渠道
  max_output_tokens: 65536

prompts:
  system: prompts/system.md
  plan_system: prompts/plan_system.md
  compress: prompts/compress.md
  skills_dir: skills
  templates_dir: prompts/templates

agent:
  recon_agents: 4
  audit_agents: 12
  infinite_mode: true           # 无限模式：不自主结束
  budget_hours: 3               # 与 budget_minutes 相加；0/0 = 不限
  budget_minutes: 0
  budget_tokens: 0              # 0 = 不限
  compress_at_ratio: 0.70
  compress_buffer_tokens: 32768 # 省略则 (context-output)/2 与 output 取小
  max_tool_result_chars: 12000
  max_tools_per_turn: 1        # 1 = 严格逐个；2-3 = 允许批量只读调用
  max_turns: 0                  # 仅普通模式生效
  retry_attempts: 3
  session_dir: sessions
  log_session: false
  log_session_dir: log_sessions
  moderator_enabled: true
  moderator_interval_seconds: 60
  forum_wait_seconds: 60
  auto_plan: true
```

## logging

```yaml
logging:
  enabled: false        # 默认关闭；开启后写入 dir/app-<时间戳>.log
  level: info           # debug | info | warn | error | off
  dir: logs
  max_size_mb: 32       # 超过即轮转（app-….1.log）
  keep_files: 5
  llm_dump: false       # true：失败的模型请求（已脱敏）落到 logs/dumps/*.json
```

- 日志只写文件，不写 stderr（TUI 独占终端）；启动时会打印日志路径。
- 所有级别都会脱敏：`sk-…`、`Authorization: Bearer …`、`api_key` 字段在落盘前被替换成 `***`。
- `llm_dump` 是排查厂商协议差异的关键：失败请求的完整 payload + 错误原文会落成 JSON。
- 运行时可用 `/log` 查看路径与级别，`/log debug|info|warn|error|off` 调整。

## 环境变量

| 变量 | 用途 |
| --- | --- |
| `GLM_API_KEY` / `MINIMAX_API_KEY` | 渠道 `api_key_env` 引用的 key；`run.sh` 会 source `.env` |
| `GOCACHE` / `GOPATH` / `GOTMPDIR` | 沙箱或 `$HOME` 不可写时指向仓库内目录 |

## 校验规则与常见报错

| 报错 | 原因 |
| --- | --- |
| `providers.X.openai.base_url is required` | 渠道没写 base_url，且没有能用预设填充的 vendor/plan |
| `providers.X.openai.model is required` | 同上，缺模型名 |
| `... token limits must be positive with output below context` | `max_output_tokens >= max_context_tokens` 或未填 |
| `openai context cannot fit output plus compression/tool/notification reserve` | 上下文太小（如 64k）而 `compress_buffer_tokens` 太大；调小或删掉让其自动推导 |
| `at least one provider must be enabled` | 全部 `enabled: false` |
| `providers.X.plan "xxx" 未知` | plan 名不在预设表 |
| `schedule.windows[i].start 必须是 HH:MM` | 时间格式写错（`9pm` 不行） |
| `schedule.timezone ... 无效` | 不是 IANA 时区名 |
| `concurrency.initial_in_flight must be between min_in_flight and max_in_flight` | 三者自相矛盾 |
