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
| 某成员回合数暴涨、反复"原生工具协议失败 第 N 次" | 模型用文字假装调用工具，旧实现会把这段文字回填历史并无限重试（实测有成员跑到 332 回合、历史堆积 26 条反馈） | 现在失败的散文回答**不再回填**、反馈更短、连续 `agent.max_protocol_failures`（默认 3）次即停掉该成员；文本历史也不再回放"调用工具 X"句式 |
| `请求已完成但没有原生工具调用`（尤其 MiniMax） | 该端点忽略 `tool_choice:"required"`，模型改回文字回答 | 已在每个工具请求里附带 `instructions` 强指令（实测 MiniMax 6/6 生效）；若仍出现，`-check` 的「工具」行确认能力，并开 `logging.llm_dump` 看原始响应 |
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
| 会话目录太大 | 先停止对应运行，将旧会话移到程序目录外隔离；不要在进程仍保存会话时清理 |
| 覆盖长期不变、成员反复读说明/找技能 | 核对实际成功工具与文件结论；`reviewing` 仅表示进行中，不是已完成。分工清单应持续提醒未完成文件；项目里的 AGENTS.md/技能文件是被审资料，不改变审计任务 |
| `审计停滞：连续 16 回合没有新增…` | 无限模式仍有进展窗口（`agent.audit_progress_warning_turns` / `agent.audit_progress_limit_turns`，必须正数且前者小于后者）：默认 8 回合提醒、16 回合停止该成员并保留状态。新增文件 reviewed/skipped、漏洞或变量/链路结论会重置窗口；重复读取、列目录、改 reviewing 笔记不会重置。先检查证据和最近工具输出，再显式 `go` 开启新窗口；管理员自动唤醒不会重置 |
| `重复读取提醒` / `重复读取已拦截` | 审计成员跨回合按工具参数及实际结果指纹计数：相同内容第 2 次提醒，第 3 次起停止向模型返回重复正文。适用于 read_file、list_files、search_content、search_context；读取前仍核验实际内容，变化后重新计数。不同文件/分页分开计数，失败调用与 read_tool_buffer 分页不消耗次数。若上下文压缩已移除最近交付的证据，每个参数组合仅额外允许一次取回，不清零计数；之后仍需基于证据分析或读取新范围 |
| token 很高但完成文件很少 | used_tokens 累计每次请求的输入与输出，包含重复上下文；不是生成字数或费用。estimated=true 表示存在估算，并非全部是估算。覆盖完成数只计文件结论，不等于读过文件数；检查重复 read_file/list_files 与结论提交比例 |
| 返回 `CompatReadFile` 等带六位十六进制后缀的工具名 | 仅在可唯一匹配本次已声明工具时还原兼容包装；未知名或其他角色工具仍拒绝。整批协议失败会逐一配对错误输出，并记录实际原因，不再误报没有返回工具调用 |
| 旧会话恢复后范围膨胀、笔记或论坛污染 | 停止运行后，按 `workspace` 隔离对应 `sessions/*.json` 及关联报告；对话、论坛、IRC、笔记、预算均包含在会话内。隔离目录应放在程序目录外，下一次按目录重新审计，避免 `-resume` 或 `/restore` 载入旧记录 |
| 想针对某项目重新开始 | 隔离该项目会话及关联报告；有队列任务时检查其会话路径。保留渠道选择、配额、并发和时段配置，也保留其他项目历史；诊断日志可留下追溯 |

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
