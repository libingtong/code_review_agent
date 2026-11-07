# 渠道、余量与故障切换

代码：[internal/provider](../internal/provider)（`provider.go` 池与状态、`quota.go` 窗口、`probe.go` 探针、
`router.go` 选源与换源、`limiter.go` 并发、`quota_manager.go` 轮询与展示、`classify.go` 错误分类）。

## 渠道（channel）是一等公民

一个 `providers[]` 条目就是一份订阅：

```yaml
providers:
  - id: glm
    label: GLM Coding Plan
    vendor: glm            # 端点/探针/费率默认值
    plan: glm-max          # 容量档位
    enabled: true
    weight: 1              # 同余量时的优先级
    openai:                # 自包含，不继承任何全局段
      base_url: https://open.bigmodel.cn/api/coding/paas/v4
      api_key: ""
      api_key_env: GLM_API_KEY
      model: glm-5.3
      max_context_tokens: 200000
      max_output_tokens: 65536
      thinking: {type: enabled}
      reasoning_effort: high
      timeout_seconds: 300
      stream: true
    quota:
      min_headroom: 0.10
      max_usage_ratio: 0.9
    schedule:              # 可选：覆盖全局时段
      enabled: false
```

规则：
- **没有全局默认值**：`base_url` / `model` 缺失直接报错（`vendor`/`plan` 可以替你填）。
- 同一厂商可开多个渠道（不同 key / 不同套餐），**窗口、消耗、上限、时段全部独立**。
- 没配 key 的渠道在 `NewPool` 阶段被自动停用并记录原因（`DisabledReason`），其余渠道照常工作；全部没 key 时仍可启动（先跑通再补）。

## 5 小时窗口

两家套餐的规则一致：**首次使用起算 5 小时、到期归零、未用完不累积**。

- `start` 在第一次成功请求记账时写入，`resetAt = start + window`；
- `advanceLocked` 只看 `resetAt`：到期即归零（曾因要求 `start` 非零导致"永久耗尽"）；
- 状态落 `state/quota.json`；**恢复只恢复消耗**，容量/单位一律以配置或 plan 为准（换套餐不会残留旧容量）；
- 容量 `ceiling = min(total, total*max_usage_ratio, total-reserve_units)`，`Ready()` 用 `ceiling` 判断，`min_headroom` 也按 `ceiling` 算比例。

## 余量观测：四层信号

| 层 | 实现 | 失败时 |
| --- | --- | --- |
| 被动 | 真实请求返回 429/欠费 → `quarantine` | — |
| http 探针 | `probe.type: http`，`bind` 支持多候选路径、`[field=value]` 过滤、`[n]` 下标、`=100` 字面量 | 降级本地计量，不阻断 |
| command 探针 | `probe.type: command`（MiniMax 用 `mmx quota --output json`） | 同上 |
| 本地计量 | token × `credits_per_million_tokens`（默认 GLM-5.3-Flash，515/M）+ 非高峰折扣 | 未知容量视为可用 |

细节：
- `probe.unit: percent`：厂商只给百分比时（GLM 的 `TOKENS_LIMIT`），按渠道容量折算成自己的单位，自设上限仍按 credits 判定。
- 响应可能是 `HTTP 200 + {"code":401,...}`：`vendorError` 会把业务错误提出来，`/quota <id>` 显示原始响应。
- `bind.plan` 可抓档位（GLM 的 `data.level`），与配置的 `plan` 不一致时 `/providers` 会提示。

## 选源与换源

```
nextProvider(excluded):
  1. 当前 active 可用（窗口 Ready + 渠道时段开）→ 继续用它（保护缓存命中）
  2. 否则在可用渠道里按 余量比例 → weight → id 排序
  3. 都不行 → nil（调用方决定暂停）
```

```yaml
routing:
  switch_below: 0.15        # 低于 15% 余量才考虑换
  min_dwell_seconds: 300    # 最短驻留，防抖动
  prefer: sticky
```

错误分类（`classify.go`）决定动作：

| 类别 | 触发 | 动作 |
| --- | --- | --- |
| `billing` | 欠费/额度类文案 | 有 reset hint → `markExhausted(until)`；无 hint → 只 `park` 5 分钟 |
| `rate_limited` | 429 | 降并发 → 换源 → 退避重试；见下 |
| `auth` / `context` / `model_not_found` | 401/403、上下文超限、模型名错 | 不换源，直接暴露（换源只会浪费另一份额度） |
| `network` | 断连/5xx/408 | 交给 `agent/retry.go` 指数退避 |
| `protocol` | 原生工具协议错误 | 由 `Agent.chatStream` 写回纠正提示并重试 |

## 429 与自适应并发（AIMD）

订阅套餐的并发上限远小于团队规模（默认 4+12 个 agent），所以每个渠道一个限流器：

```yaml
concurrency:
  max_in_flight: 0          # 0 = recon+audit+1
  initial_in_flight: 4
  min_in_flight: 1
  increase_after_successes: 20
  increase_step: 1
  decrease_factor: 0.5
  increase_cooldown_seconds: 120
```

- 限的是**在飞请求数**：agent 在 `Acquire` 处等待，上下文不丢；
- 429：该渠道并发减半 → 若另一渠道可用则切换 → 否则按 `retry-after`（缺省 2s×次数，封顶 60s）退避后重试同一渠道；
- **并发到底（min）仍 429，或连续 8 次 429** → 判定为套餐额度问题，`quarantine` 后暂停；
- 成功每 20 次 +1；被限流后 2 分钟冷却期内不增长。

## 稳定后固化并发（学习值）

每次启动都从 `initial_in_flight` 重新试探，会把已经解决过的 429 再撞一遍。所以限流器会把**稳定下来的并发记住**：

```yaml
concurrency:
  learn: true               # 默认开：稳定后记住
  pin: false                # true = 固定在学习值，不再向上试探
  settle_successes: 40      # 连续多少次成功算"稳定"
  state_file: state/concurrency.json
```

机制：
- 每次成功累加 `stable`；期间若发生 429 或向上增长，`stable` 归零（只有"没变过"才算稳定）；
- `stable >= settle_successes` 且当前值与已学值不同 → 记为学习值、落盘、并在事件流里提示 `glm 并发稳定在 3，已记住`；
- **下次启动直接以学习值起步**（会被 `min_in_flight`/`max_in_flight` 夹紧）；
- 429 仍然会让它减半；若减半后重新稳定，学习值被更新为更低的那个；
- `pin: true` 时以学习值为上限，不再向上增长（适合"套餐并发很紧、只求稳定"的场景）；
- 清除：`/concurrency reset [ID]`，或删 `state/concurrency.json`。

这个值属于**厂商/套餐**，与 5H 窗口无关，所以单独存一个文件。

## 协议选择（chat vs responses）

两个渠道都支持两种协议，实测结果：

| 端点 | Chat Completions | Responses | `previous_response_id` |
| --- | --- | --- | --- |
| GLM | `https://open.bigmodel.cn/api/coding/paas/v4` | `https://open.bigmodel.cn/api/v1` | **不支持**（返回 `not_found`） |
| MiniMax | `https://api.minimax.cn/v1` | 同 base 的 `/responses` | 支持 |

推荐都用 `api_interface: responses`：原生 `tool_choice`、能拿到 reasoning 文本、按响应 id 记 lineage。

- 续接是**能力协商**的：GLM 拒绝 `previous_response_id` 时，客户端丢弃 lineage、重发完整历史，
  并记住该渠道不再发续接——所以切到 responses 不会让 GLM 每回合报 404；
- 能力只在配置里声明协议，不需要额外开关；`-check` 会真实探测工具调用与模型名。

## 并行工具调用与历史回放

订阅厂商的 Responses 实现未必完全按 OpenAI 语义来，两处实测差异已在客户端兜住：

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `模型返回 2/3 个原生工具调用；每回合只允许一个，整批未执行` | 厂商一次返回并行调用（`parallel_tool_calls:false` 不一定被遵守） | 不再整批丢弃：**执行第一个**，其余补一条"本回合跳过"的 `function_call_output`，保证每个 call_id 都有对应结果 |
| `invalid_request messages 参数非法` / `response.failed` | 端点接受 tools 出去，但拒绝我们把 `function_call` / `function_call_output` 原样回放 | 该请求失败一次后，把历史降级成纯文本（`[上一回合调用工具 X，参数 …]` / `[工具结果] …`）并**记住该渠道**，后续回合都用文本历史 |

降级只影响"怎么把历史讲给模型听"，工具本身仍走原生调用；`-check` 的「工具」行仍会显示原生工具是否可用。

### 语言模型的"自觉"不靠谱：`instructions` 强指令

实测结论（两家都测过）：

| 厂商 | `tool_choice: "required"` | 加系统级 `instructions` 强指令后 |
| --- | --- | --- |
| GLM `/api/v1` | 遵守 | 遵守 |
| MiniMax `/v1` | **静默忽略**（用 3 组"请用文字总结/闲聊"的提示词验证，全部回文字） | 6/6 都改成 `function_call` |

所以每个带工具的 Responses 请求都会附带一段固定指令（`toolDirective`）："本回合必须通过原生
function call 调用工具，禁止只用文字回答；没有明确动作也要调用最相关的只读工具"。这是
"请求已完成但没有原生工具调用"这类失败的**唯一**可靠解法——提示词层面的软约束会被模型在
长上下文里漂移，per-request 的 `instructions` 不会。

## 原生工具调用与工具能力自检

agent 只走原生 function calling，所以协议层必须"逼"模型调工具，而不是等它自愿：

- **chat 协议**：请求体带 `tool_choice: "required"`（此前没带，模型可能整段文字作答，
  触发「请求已完成但没有原生工具调用」）；
- 个别端点不认 `tool_choice: "required"`（返回 4xx 且报错提到 tool_choice）：自动回退成
  `"auto"` 并**记住该渠道的能力**，后续请求不再重复撞 400；
- **responses 协议**：本来就带 `tool_choice: "required"`。

`-check` 会真实探测一次：

```
工具 glm-5.3-flash：原生 function_call OK
工具 xxx：未返回 function_call ← 该模型在这个端点上可能不支持工具调用；模型回答开头：……
```

探测用的是内置 `noop` 工具 + 一句"必须用 function_call 调用"。看到后半句就说明该模型/端点
不支持工具调用，应换模型或换协议；agent 侧在连续两次协议失败后也会给出同样的提示。

## 选择使用的渠道/模型（`/mode`）

```text
/mode                      打开选择器：↑↓ 选渠道，Enter 使用该渠道与它的模型
/mode glm                  只切渠道（沿用该渠道当前的模型）
/mode glm glm-5.3-flash    切渠道并换模型（运行时重建客户端）
/mode auto                 清掉选择，下次启动按 weight 重新决定
```

- 选择写入 `state/selection.json`（`routing.selection_file`），**下次启动自动沿用**；
- 选择只记录"手动"意图：运行中因为余量/429 自动切换不会覆盖它（重启后仍回到你选的渠道）；
- 选中的渠道若在新一次启动时被停用/不存在，会被忽略并在事件流提示，不会启动失败；
- `/mode auto` 同时清除模型覆盖，回到配置里的模型。

## 自设用量上限

```yaml
quota:
  max_usage_ratio: 0.9    # 只用窗口的 90%
  reserve_units: 0        # 或按绝对值保留
```

- 到上限该渠道标记 `已达自设上限`，路由换源；两源都到上限 → 暂停（原因与"厂商额度耗尽"区分）；
- 运行时：`/limit glm 90%`、`/limit glm reserve 500`、`/limit glm off`，持久化到 `state/limits.json`。

## 跨渠道续接

`previous_response_id` 是厂商私有的。池子维护 `lineage[responseID] = providerID`，切换渠道后自动
`DropResponseContinuation`，改为发送完整历史（一次性的 token 成本，换来正确性）。

## 诊断入口

| 入口 | 输出 |
| --- | --- |
| `-check` | 每渠道：余量/状态/重置时间/并发 + **端点可用模型列表** |
| `/providers` | 渠道卡片：vendor/plan、余量、时段、并发（含学习值）、429 次数、最近错误、停用原因 |
| `/concurrency` | 各渠道当前/学习并发与 429 次数；`/concurrency reset [ID]` 清除学习值 |
| `/quota [ID]` | 强制刷新；带 ID 时打印**原始响应**，用于修正 `bind` |
| `ExhaustionReport()` | 无可用渠道时逐条说明原因（时段外/冷却/自设上限/额度耗尽/已停用），并带进错误与暂停事件 |
