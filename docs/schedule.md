# 工作时间段

代码：[internal/schedule](../internal/schedule)（`window.go` 单个窗口、`plan.go` 两层策略、`gate.go` 门控、`store.go` 持久化）。
消费方：`agent.Team`（启动前准入）、`orchestrator.Runner`（任务之间等待）、`internal/tui`（编辑器与命令）。

## 两层策略

```
Plan
 ├─ global   Schedule          ← schedule.windows / schedule.enabled
 └─ channels map[id]Schedule   ← providers[].schedule（存在即覆盖全局）
```

```yaml
schedule:                       # 全局：夜里跑
  enabled: true
  timezone: Asia/Shanghai
  windows:
    - {start: "21:00", end: "08:00", days: [mon, tue, wed, thu, fri, sat, sun]}
  on_window_close: pause        # 或 finish_current_task
  poll_seconds: 30

providers:
  - id: glm
    schedule:
      enabled: false            # 覆盖：忽略全局，全天可用
  - id: minimax                 # 不写 schedule → 跟随全局
```

- **渠道覆盖优先**：`enabled: false` 表示"该渠道不受时段限制"；`enabled: true + windows` 表示"该渠道用自己的窗口"。
- **只要还有任一渠道开窗，程序就不会整体暂停**；路由器也只挑当前开窗的渠道（时段外的渠道即使余量充足也不会被调用）。
- 删除全局最后一个窗口会自动关闭全局限制，避免"永远暂停"。

## 窗口语义

- `21:00-08:00` 视为**跨午夜**：窗口归属**开始那天**，因此周五 22:00 的窗口覆盖到周六 06:00，周六夜里不生效。
- `days` 缺省 = 每天；支持 `all` / `weekdays` / `weekend` / `mon-fri` / `mon,wed,fri`（编辑器里可写成这些）。
- 时区用 IANA 名（`time.LoadLocation`），DST 当天仍按本地墙上时间判定。
- `NextOpen` 从当前时刻向后最多扫 8 天，取最早的开启点。

## 门控判定（`Gate.Check`）

```
1. 时段：Plan.AnyOpen(now) == false → PauseWindow（ResumeAt = Plan.NextOpen）
2. 余量：pool.EarliestReset() 有值 → PauseQuota（Reason/Detail 来自渠道状态）
3. 否则 Allow
```

`Decision{Kind, Allowed, Reason, ResumeAt, Detail}`，其中 `Detail` 是逐渠道原因，会出现在：
- `Team.Run` 启动前的 `paused` 事件；
- 运行中模型层返回 `provider.ErrOutOfHours` 时 `modelDisconnected` 生成的暂停提示；
- 编排器 `waitForGate` 的 "各渠道状态：…" 日志。

## 暂停与恢复

| 场景 | 行为 |
| --- | --- |
| 启动时不在时段 | `Team.Run` 直接返回，记录 `PauseInfo`，不创建 worker |
| 运行中跨出时段 | 路由器返回 `ErrOutOfHours` → `retry` 走暂停通道 → 团队取消但保留进度 |
| 交互式 | 提示预计恢复时间；`go` 是显式豁免（仍会先过门控，除非强制恢复） |
| 任务队列 | `waitForGate` 阻塞等待，到点自动继续；`on_window_close: finish_current_task` 表示先跑完当前任务 |

## 运行时编辑

```text
/window                          多窗口编辑器：a 新增、Enter/e 编辑、d 删除、空格 开关
/window 22:00-06:00 mon-fri      直接设全局时段
/window off | on                 关闭/开启全局时段
/window glm always               渠道覆盖：全天可用
/window glm inherit              渠道恢复跟随全局
/window glm 21:00-02:00 all      渠道自己的窗口
```

改动**立即生效**并写入 `schedule.store_file`（默认 `state/schedule.json`），**启动时优先于配置文件**。
`state/schedule.json` 保存的是完整 `Policy`（全局 + 所有渠道覆盖），格式见 `store.go` 的 `policyFile`。

## 持久化格式

```json
{
  "version": 1,
  "policy": {
    "enabled": true,
    "timezone": "Asia/Shanghai",
    "windows": [{"start": "21:00", "end": "08:00", "days": ["mon", "..."]}],
    "channels": {
      "glm": {"enabled": false, "timezone": "Asia/Shanghai", "windows": []}
    }
  }
}
```

`enabled: false` 的渠道条目代表"忽略全局"，即使 `windows` 为空也合法。
