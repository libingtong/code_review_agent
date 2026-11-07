# 排障手册

先跑这一条，八成问题能自证：

```bash
./code-review-agent -config config.yaml -check
```

它会打印：每个渠道的余量/状态/重置时间/并发、探针结果、**端点可用模型列表**、时段状态、任务队列。

## 症状 → 原因 → 动作

| 症状 | 常见原因 | 动作 |
| --- | --- | --- |
| `找不到模型` / `model not found` | `providers[].openai.model` 名字与端点不符 | `-check` 的 `模型 xxx：可用 …` 一行会列出合法名字；不在列表时会标 `← 当前配置的名字不在列表中` |
| `所有源余量均不足：glm: 已停用（未配置 API key（GLM_API_KEY））` | 该渠道没 key，被自动停用，剩下唯一渠道刚好也停了 | 填 `.env` 的 `GLM_API_KEY` 后重启；`/providers` 里按 `e` 可临时启用 |
| `所有源余量均不足：minimax: 时段外` | 不是额度问题，是工作时间段把它关了 | `/window mm always` 或 `/window off`；`-check` 的「时间窗」一行会显示每个渠道的开/关 |
| `所有启用源均在限流冷却中，稍后自动重试` | 429 触发降档，或单次欠费被 `park` 5 分钟 | 等冷却；`/providers` 看 `并发 x/y · 429 次数`，必要时调 `concurrency` |
| `请求已完成但没有原生工具调用（回答 N 字节…）` | 模型用文字回答而没有发 `function_call`；chat 协议缺 `tool_choice` 时常见 | 已在协议层强制 `tool_choice: "required"`；若仍出现，`-check` 的「工具」行会显示该模型是否支持工具调用，据此换模型或改 `api_interface: responses` |
| `模型返回 N 个原生工具调用；每回合只允许一个` | 厂商返回并行调用；旧实现整批丢弃 | 现在执行第一个、其余标记跳过；若频繁出现可把 `parallel_tool_calls` 关掉（已默认关，部分厂商忽略） |
| `invalid_request messages 参数非法` / `response.failed` | 端点拒绝回放 `function_call` 历史项 | 客户端自动降级为文本历史并记住该渠道；同一请求失败一次后不再复现 |
| 频繁 429，变慢 | 团队并发 > 套餐允许 | 正常行为：并发减半后逐步回升；想更保守可把 `concurrency.initial_in_flight` 调小 |
| `mmx: executable file not found` | MiniMax 余量探针依赖官方 CLI | 可忽略（退化为本地计量），或 `npm i -g mmx-cli && mmx auth login --api-key sk-cp-...` |
| 探针 `code=401 令牌已过期或验证不正确` | key 用错类型（标准 API key ≠ 编程套餐 key）或过期 | 去「个人编程套餐 → 套餐概览」新建 key 填 `GLM_API_KEY` |
| 探针 `未命中任何 bind 路径` | 接口字段变了 | `/quota glm` 看原始响应，改配置里的 `probe.bind`（多候选路径、`=100` 字面量） |
| 启动即报 `openai context cannot fit output plus compression/tool/notification reserve` | 上下文太小（如 64k）而 `agent.compress_buffer_tokens` 太大 | 调小该值，或删掉让它按 `(context-output)/2` 自动推导 |
| 启动即报 `providers.X.openai.base_url is required` | 去掉了全局默认值后渠道没写全 | 补 `base_url`/`model`，或改用 `vendor` + `plan` |
| `/tasks run` 不动 | 时段关闭或所有渠道不可用（队列在等门控） | 看事件流的「暂停：…」；`/window off` 或等窗口 |
| `/tasks` 里有任务一直 `paused` | 余量暂停，不消耗重试次数 | 恢复后会原地续跑；`/tasks retry <ID>` 可强制重排 |
| 恢复会话后没反应 | 需要手动继续 | `/session` → Enter 恢复 → 输入 `go` |
| 恢复会话报「未找到会话」 | 片段不唯一或路径不对 | `/restore` 不带参数列出全部；或用完整路径 |
| TUI 花屏/串行 | 终端宽度不足或 CJK 宽度判定 | 放大终端；`RUNEWIDTH_EASTASIAN` 未显式覆盖时按单宽处理 |
| `-check` 里 `reset=—` | 探针没返回重置时间（正常） | 无害；本地计量会在首次使用后自建窗口 |

## 三个"刹车"要分清

暂停原因里出现的时间/措辞直接对应不同机制，别混：

| 现象 | 机制 | 解除方式 |
| --- | --- | --- |
| `时段外` | 工作时间段（全局或渠道覆盖） | 到点自动恢复；`/window` 调整 |
| `冷却至 HH:MM` | 429 或未确认的欠费错误（`park` 5 分钟） | 自动；或切到另一渠道 |
| `已达自设上限 90%` | `max_usage_ratio` / `/limit` | 到窗口刷新；`/limit glm off` |
| `额度耗尽（重置 …）` | 探针确认窗口耗尽，或厂商给了 reset hint | 等重置；或换渠道 |

## 数据/状态类问题

| 现象 | 处理 |
| --- | --- |
| 余量显示明显不对（比如明明充足却 0%） | 删 `state/quota.json` 后重启（窗口会重建）；仍不对则 `/quota <id>` 看原始响应 |
| 自设上限改不回去 | `/limit <id> off`，或删 `state/limits.json` |
| 时段怎么改都没生效 | 检查 `state/schedule.json`（它的优先级高于 config.yaml）；删掉即回到配置 |
| 会话目录太大 | `sessions/*.json` 每个几十 MB（保存了所有 worker 对话），可删旧文件 |
| 想彻底重来 | 删 `state/`、`sessions/`、`orchestrator/`、`reports/`（都是运行态，不入库） |

## 抓日志（推荐先做这个）

```yaml
logging: {enabled: true, level: debug, llm_dump: true}
```

重跑一次，然后把这两样带上来：

- `logs/app-*.log`：路由、阶段、429/暂停、失败请求的错误原文；
- `logs/dumps/*.json`：**失败请求的完整 payload（已脱敏）** ——厂商拒绝的具体字段就在里面。

TUI 里 `/log` 会显示当前文件路径；`/log debug` 可以临时提级，不必重启。

## 报 bug 时请附带

1. `-check` 的完整输出（含「探针」「模型」两行）；
2. `/providers` 里相关渠道的卡片（余量、时段、并发、最近错误、停用原因）；
3. 事件流里第一条 `暂停/错误` 文案；
4. `go test ./internal/provider/ -run TestFailsOver -v` 的结果（确认不是环境个例）。
