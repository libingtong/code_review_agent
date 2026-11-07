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
