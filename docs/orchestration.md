# 任务编排

代码：[internal/orchestrator](../internal/orchestrator)（`queue.go` 任务与持久化、`runner.go` 执行循环）。
入口：TUI `/tasks ...`，或 `orchestrator.enabled: true` 后由 `/tasks run` 驱动。

## 队列模型

```yaml
# tasks.yaml
tasks:
  - id: T001                 # 可省略，导入时按序号生成
    title: 智通人才
    dir: /path/to/repo
    prompt: 先看鉴权与上传     # 可省略，默认"请对当前工作区进行安全审计…"
    budget: {hours: 3, minutes: 0, tokens: 0}
    agents: {recon: 4, audit: 12}   # 预留字段
    retries: 1               # 0=继承 orchestrator.retries；负数=不重试
```

三种导入方式：

| 输入 | 说明 |
| --- | --- |
| `.yaml` / `.yml` / `.json` | `{tasks: [...]}` 或裸数组 |
| `.txt` | 每行 `目录` 或 `目录 \| 提示词`，`#` 开头为注释 |
| 目录 + `--each-subdir` | 每个子目录一个任务（否则整个目录算一个任务） |

导入时按 `dir` 去重；不存在的目录直接报错。

## 状态机

```
pending ──run──▶ running ──完成──▶ done
   ▲                │
   │                ├─ 余量/时段暂停 ─▶ paused（不消耗重试次数，会话已保存）
   │                ├─ 未完成（有重试）─▶ pending（attempts+1）
   │                └─ 未完成（重试耗尽）─▶ failed
   └── /tasks retry ── failed/paused/skipped
       /tasks skip  ──▶ skipped
```

状态文件 `orchestrator.state_file`（默认 `orchestrator/state.json`），记录 `cursor + tasks`；
进程被杀后重新打开，`normalize()` 会把 `running/paused` 归为 `pending`，从游标继续。

## 单个任务的执行顺序（`runTask`）

```
1. waitForGate        ← 时段 + 余量（Gate.Check），阻塞直到允许
2. team.SetWorkspace(task.Dir)
3. team.ConfigureBudget(task.budget 或全局默认)
4. sessions/tasks/<ID>.json 存在 → team.LoadSession(...) 并把 prompt 前缀改为"继续基于当前团队状态…"
5. team.Run(ctx, prompt, emit)
6. 结束后按结果分支：
   - Phase()=="completed"      → SaveSession + 导出 reports/<ID>/report.{json,md} → done
   - PauseInfo() 非空          → SaveSession → paused（attempts--，不消耗重试）
   - ctx 取消                  → pending（进度保留）
   - 其它                      → attempts>retries ? failed : pending
```

约束：
- **串行**：`Team.Run` 本身单飞，所以并发固定为 1（`concurrency` 字段留作未来扩展）。
- **失败不阻塞后续**：`continue_on_failure: true` 时继续下一个任务；`false` 时停在失败处。
- 每任务产出独立目录，互不覆盖。

## 产出

```
reports/T001/report.json     ← internal/report.Build + TaskInfo{id,title,dir}
reports/T001/report.md       ← 人读版（漏洞表 + 覆盖 + 撤销记录 + 项目笔记）
sessions/tasks/T001.json     ← 断点（可被 /restore 打开，但别与 /tasks run 同时操作同一 Team）
```

## 与门控/余量的关系

- 队列是一等"无人值守"路径：`waitForGate` 会在时段关闭或全部渠道耗尽时**原地等待**，到点自动继续。
- 交互式审计（直接输入目录）不自动重排；它以 `paused` 事件告知原因，用户 `go` 或等时段开窗后手动继续。

## 常见改动

| 需求 | 位置 |
| --- | --- |
| 新增导入格式 | `queue.go` 的 `importPath` 分支 |
| 改重试/失败策略 | `runner.go` 的 `runTask` 末尾分支、`OrchestratorConfig` |
| 加任务级字段（agents/budget 之外） | `Task` 结构 + `runTask` 读取处 + `docs/configuration.md` |
| 并行执行多任务 | 需要多个 `Team` 实例；`Team.Run` 单飞是硬约束，改动面较大 |
