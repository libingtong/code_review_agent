# 下一代Agent代码审计
简短的来说，我不太喜欢现在开源的大部分的"代码审计"系统的复杂性和花里胡哨的UI.因此我开发了这套代码审计agent系统并且用于测评我的QWEN-EXO项目 

>《qwen-exo - 让 Qwen 在长任务里真正记住知识、反思错误，并跑得足够快》
https://github.com/huoji120/QWEN-EXO-booster

它具有以下特点
1. 完美适配QWEN-EXO,支持高速高效离离线部署,你只需要两台DGX就行.(外接模型也可以,也完美适配deepseek)
2. agent跟团队一样在研究问题,模型之间会互相配合,他们会像一支专业团队一样,在内部地下论坛进行交流,记笔记,分工合作,分享成果,一起挖漏洞.
3. 不依赖第三方库,编译即可用,高效简洁,我非常讨厌乱七八糟的WEBUI以及花里胡哨的东西,简洁才好用.
4. 超大型项目实战,已经过大型项目测试,本项目为超大型项目审计而生.在内部大型项目,V8引擎项目中发现了许多致命/非致命漏洞.在2026年年初的V8中,发现了ppc/s390，CmpS32>CmpU64，perf-jit 跨脚本内联 UAF，js-objects属性!=/== 反转 等一系列高危安全问题。
5. 非常简单,简洁,易用,效果非常足,好不好用,自己用一下.

# 多源订阅与无人值守（本分支新增）

一份订阅跑不完大项目，两份又不想盯着。这一版把"用哪份订阅、什么时候跑、跑什么"都交给程序：

| 能力 | 一句话 | 配置/命令 |
| --- | --- | --- |
| 多源池 | GLM 与 MiniMax 同时接入，按 5 小时窗口余量自动分配 | `providers[]` |
| 5H 余量探测 | 主动探针 + 官方 CLI + 本地计量 + 429 被动信号，四层兜底，探针挂了不停审计 | `/quota`、`-check` |
| 渠道与套餐 | 额度是**渠道**属性：同厂商多个 key/套餐各算各的，`plan` 决定容量档位 | `vendor`、`plan` |
| 自设用量上限 | 只用窗口的 90%，剩下留给 Claude Code 等其它工具；到上限自动换源 | `max_usage_ratio`、`/limit` |
| 自动换源 | 余量不足才切（默认 15% 阈值 + 5 分钟驻留），保护 97%+ 缓存命中 | `routing` |
| 并发自适应 | 频繁 429 说明并发过高：该源并发减半，成功后再逐步加回 | `concurrency` |
| 工作时间段 | 全局一套窗口，渠道可覆盖（例如 GLM 全天、MiniMax 只在夜里） | `schedule`、`/window` |
| 任务编排 | 一批目录/任务导入后串行跑完，断点续跑，每任务独立报告 | `/tasks` |
| 非交互自检 | 不进 TUI 就能看源、余量、时段、队列 | `-check` |

示例配置：`config.multi.yaml`（双源 + 时段 + 编排）；现成的 `config.yaml` 也已按"全局 `openai` 默认值 + `providers` 渠道列表"组织好。
细节见下方「多源订阅管理」一节。

# 分支规则

- **`main`**：唯一开发分支，所有提交都落在这里。
- **`master`**：只做 `main` 的快进镜像，不在其上直接开发，也不允许分叉。
- 同步命令（在 `main` 上、工作区干净时执行）：

```bash
./scripts/sync-master.sh     # 快进 master → 推送 main 与 master
```

脚本用 `git fetch . main:master` 做**只允许快进**的更新，一旦两边分叉会直接报错而不是悄悄合并；如果你在 `master` 上直接提交过，先把它 rebase/merge 回 `main` 再同步。

# 相关截图
![img](img/3.png)

![img](img/4.png)

# 支持模型
我是自己用于本地离线代码审计的,建议使用QWENEXO作为本地推理架构,并且使用QWWEN-27B系列模型.
如果没条件,推荐使用deepseek-flash/glm5.3,不虚mythos.

# 技术介绍

## 多阶段分工合作,避免上下文污染
此agent使用多阶段分工合作,覆盖侦察->研究->利用阶段(公开版不带利用阶段),用于解决上下文污染的问题,具体参考问题:
《[2026]从模型注意力角度创建一个简单但高效的Agent》
https://key08.com/index.php/2026/08/10/3275.html

## 你的agent跟团队一样在研究问题
在测试QWEN-EXO代码审计的研究中,注意到agent数量和任务完成率成正比
![img](img/7.png)

而这个居然是遵循《Scaling Law》的

![img](img/5.png)

因此我开发了一个多agent协作系统.负责模型之间的互相配合,他们会像你的团队一样,在内部地下论坛进行交流,记笔记,分工合作,一起解决你的问题
![img](img/6.png)

他们会独自分工,讨论问题,研究漏洞是否存在:
![img](img/8.png)

## 高效缓存,极致性价比
不做乱七八糟的提示词注入等破坏性的东西,缓存基本在97%-99%左右

## 长任务续接与网络故障恢复

- Responses API 的每个 Agent 保存自己的 `previous_response_id`。后续回合只提交上次成功响应之后新增的消息，避免重复发送完整历史；上下文压缩或会话恢复会安全地重置续接点并发送完整新历史。
- 单个 Agent 遇到网络断开、连接重置、服务端 5xx、408/429 或不完整响应时不会被标记为失败，也不会让其他 Agent 进入无人状态。它会继续重试，退避时间从 1 秒指数增长，最多 5 分钟；暂停或取消团队才会停止重试。
- API 余额/额度不足仍然立即停止全队；上下文超限、工具协议错误等非网络错误不会伪装成网络重试。
- 原生工具调用格式错误、缺少 `call_id`、重复调用或无效工具名不会在连续三次后永久杀死 Agent；Agent 会把具体错误和正确的 `function_call` 格式写回上下文，持续重试直到任务取消或模型恢复。合法工具调用成功后，连续失败计数归零。


![img](img/9.png)

## deepseek懒惰修复
在做这个项目中,我测试了deepseek,发现deepseek-v4.1-flash模型的上下文一旦长了就开始懒惰起来,会尽可能的想办法结束任务.因此我设计了一个投票结束系统,只有在所有agent都同意结束任务的情况下才能结束任务,否则agent会不工作.

有意思的是，deepseek的agent一个完成后，想投票关闭被拒绝后，会疯狂的骚扰其他的agent让他们结束自己的工作。因此对这块提示词做了优化，禁止一个agent去骚扰其他人，自己找活干。即便是重复的工作
普通（有限）模式下，关闭请求待定、被拒绝或超时后，每个 Agent 应继续**自己的审计工作**：自行选择其他未覆盖的文件、入口或模块，建立具体待办并读取源码、追踪新链路。不催其他成员投票，不反复请求关闭，也不靠重复复核同伴已有结论消磨时间。


## 无限猴子模式

无限模式下，审计模型没有 `end_audit` 结束工具，也不能投票收工。侦察完成后仍正常交接到审计；审计继续运行，直到用户暂停，或时间/token 任一预算耗尽。推荐在deepseek使用因为deepseek非常懒惰特别喜欢结束.导致大型项目没审计完就结束。

### 怎么开启

在你实际使用的配置文件（例如 `config.yaml` 或 `config_ds.yaml`）的 `agent:` 下加入：

```yaml
agent:
  infinite_mode: true
  budget_hours: 8
  budget_minutes: 0
  budget_tokens: 0
```

- 时间默认 **8小时**，token 默认不限；时间是 `budget_hours + budget_minutes`。
- **OR 关系**：时间到，或者累计 token 达到上限，任一发生就停止整个团队。
- 小时和分钟都为 `0` 表示时间不限；`budget_tokens: 0` 表示 token 不限。三项都为 `0` 才是不设预算上限。
- `infinite_mode: false` 是普通模式，仍使用团队共识结束；这几个预算上限不生效。

指定配置和目录启动：

```powershell
.\code-review-agent.exe -config config.yaml -dir "F:\待审计项目"
```

也可以启动程序后输入目录。无限模式会先弹出预算输入框，默认内容为：

```text
infinite 8 0 0
```

四项依次是 **模式、小时、分钟、token**。编辑后按 **Ctrl+S** 确认并开始，**Esc** 取消，不会启动。

| 输入 | 含义 |
| --- | --- |
| `infinite 8 0 0` | 最多8小时，token不限 |
| `infinite 2 30 10000000` | 最多2小时30分钟或1000万token，先到即停 |
| `infinite 0 0 10000000` | 时间不限，最多1000万token |
| `infinite 0 0 0` | 时间、token均不限，手动停止 |
| `normal 8 0 0` | 切回普通共识结束模式，预算不生效 |

停止状态下输入 **`/budget`** 可切换模式、调整预算；配置文件负责启动默认值，界面修改保存在会话中，不回写 YAML。运行中在对话框外**连续按两次 Esc** 确认暂停，等待成员完全停止后输入 **`go`** 继续。


## 多源订阅管理：5 小时余量、自动切换、工作时段与任务编排

一份订阅跑不完一个大项目，两份订阅又不想手动盯着。程序内置了一个多源池：把 GLM Coding Plan 和 MiniMax M Plan 都配进去，按各自 5 小时窗口的余量自动分配，一侧见底就换另一侧；两侧都空就暂停，到点自动接着跑。配置见 `config.multi.yaml`。

### 0. 配置：只有渠道，没有全局默认值

没有全局 `openai:` 段了。每个 `providers[]` 条目自带 endpoint / key / 模型 / 上下文，谁生效一目了然，也不会出现"模型名到底从哪继承来的"这种问题。

```yaml
providers:
  - id: glm
    vendor: glm                 # 端点、探测方式、抵扣系数由 vendor/plan 决定
    plan: glm-max               # 容量档位：2000 / 12000 / 28000 积分每 5 小时
    openai:
      base_url: https://open.bigmodel.cn/api/coding/paas/v4
      api_key_env: GLM_API_KEY
      model: glm-5.3
      max_context_tokens: 200000
      max_output_tokens: 65536
```

- 想少写字段：`vendor` + `plan` 已经能填出端点、模型、上下文、探测与费率；写了就以你写的为准。
- **没填 key 的渠道会自动标记「已停用」并跳过**，不会让程序起不来，也不会把请求打到没配 key 的端点。
- **报「找不到模型」**：`./code-review-agent -config config.yaml -check` 会直接问端点要模型列表并打印出来，不在列表里会标出来。实测两个端点：

```
模型 glm-5.3：可用 glm-4.5, glm-4.5-air, glm-4.6, glm-4.7, glm-5, glm-5-turbo, glm-5.1, glm-5.2, glm-5.3, glm-5.3-flash, …
模型 MiniMax-M3.1-Flash：可用 MiniMax-M3.1-Flash, MiniMax-M3, MiniMax-M2.7, MiniMax-M2.7-highspeed, …
```

- `compress_openai` 可省略：压缩预算自动取所有启用渠道里最紧的上下文，压缩调用本身走渠道池（享受换源）。

### 1. 快速开始

```bash
cp .env.example .env && chmod 600 .env        # 填 GLM_API_KEY / MINIMAX_API_KEY
./code-review-agent -config config.yaml -check   # 非交互自检：余量、模型名、时段、队列
./code-review-agent -config config.yaml          # 进入 TUI
```

`-check` 会打印每个渠道的余量、探针结果、**端点可用模型名**、时间窗状态和任务队列，不进入界面——排查配置最快的方式。

只有完全没写 `providers` 的旧配置才会走单源兼容路径（把 `openai` 段当作唯一渠道）。

### 2. 5 小时余量怎么来的

GLM 和 MiniMax 的套餐额度都是"首次使用起算 5 小时、未用完不累积"，但**两家都没有公开文档化的余量查询 API**。所以程序不写死单一接口，而是四层信号，优先级从高到低：

| 层 | 作用 | 配置 |
| --- | --- | --- |
| 被动信号 | 任何真实请求返回 429/欠费/额度错误，立刻把该源置为耗尽并解析恢复时间 | 无需配置，常开 |
| 探针 `http` | 直接请求控制台接口；字段路径可在 `bind` 里给多个候选 | `quota.probe.type: http` |
| 探针 `command` | 跑官方 CLI（MiniMax 的 `mmx quota` 是文档化能力） | `quota.probe.type: command` |
| 本地计量 | 用实测 token × 官方抵扣系数（默认按 **GLM-5.3-Flash**：2.3 输入 / 0.56 缓存 / 8 输出 每万 token，折算 515 积分/百万 token）+ 非高峰 5 折 | `quota.local` |

关键取舍：**探针挂了绝不停审计**。探针失败只是降级到本地计量；只有真实请求被拒才算耗尽。5 小时窗口状态落盘在 `state/quota.json`，重启不会把半个窗口当成满额。

字段对不上时：`/quota glm` 会显示原始响应和解析结果，改 `bind` 路径即可，不用改代码。

### 2.0.5 一个渠道一份 quota

额度是**渠道**的属性，不是程序的属性：同一个厂商开两个渠道（两个 key、两份套餐），它们的余量各算各的，互不影响。

```yaml
providers:
  - id: glm-max            # 大套餐，跑主力审计
    vendor: glm
    plan: glm-max          # 28000 积分 / 5 小时
    openai: {api_key_env: GLM_API_KEY}

  - id: glm-lite           # 另一份小套餐，跑轻任务
    vendor: glm
    plan: glm-lite         # 2000 积分 / 5 小时
    openai: {api_key_env: GLM_LITE_API_KEY}
    quota: {max_usage_ratio: 0.8}
```

- `vendor` 决定端点、探测方式、抵扣系数；`plan` 决定**容量档位**（GLM：lite 2000 / pro 12000 / max 28000 积分每 5 小时）。两者都写在 `providers[]` 里，所以“加一个渠道”只需要 id + vendor + plan + key。
- 显式写了的字段永远覆盖预设，不会被 plan 覆盖。
- 探测到的百分比会自动折算成渠道自己的单位（例如 max 档 25% → 7000/28000 credits），因此自设上限仍然按 credits 判定。
- 接口若返回档位（GLM 的 `data.level`），程序会与配置的 `plan` 比对，不一致时在 `/providers` 提示「接口报告 max，请核对 plan」。

### 2.1 只用自己的那一份：自设用量上限

同一份订阅可能还有别的系统在用（Claude Code、IDE 插件、同事的脚本）。可以给每个源设"我最多只用这么多"：

```yaml
providers:
  - id: glm
    quota:
      max_usage_ratio: 0.9     # 只用 90%，剩 10% 留给其它工具
      # 兜底计量费率：不写则按 GLM-5.3-Flash 的 515 积分/百万 token；
      # 若你实际跑 glm-5.3（贵档）想更保守，可显式写 credits_per_million_tokens: 1545
      reserve_units: 0         # 或者按绝对值保留（credits 计量时用）
```

运行时也能改，改完持久化到 `state/limits.json`，重启仍生效：

```text
/limit                       # 看各源当前上限与可用量
/limit glm 90%               # 或 /limit glm 0.9
/limit glm reserve 500       # 按绝对值保留 500 credits
/limit glm off               # 取消上限
```

行为细节：

- 上限**只作用于本程序**，不改变厂商计数；到上限时该源标记 `已达自设上限`，路由器自动换到另一源；
- 两个源都到上限 → 暂停，原因会明确写成"所有启用源均已达到自设用量上限（保留余量给其它工具）"，窗口刷新后自动继续；
- 上限是"可用额度"的分母：`min_headroom` 的百分比也按上限算，所以 90% 上限 + 10% headroom 意味着用到 81% 就开始让位；
- `/providers` 里显示为 `余量 62%（自设上限 90%，可用 3720 credits）`。

### 3. 什么时候换源

```yaml
routing:
  switch_below: 0.15        # 当前源余量低于 15% 才换
  min_dwell_seconds: 300    # 最短驻留 5 分钟，避免抖动
  prefer: sticky
```

保守是刻意的：换源会清空上游 prompt cache（本项目主打 97%–99% 命中），所以只在真的快见底时切。切换会出现在运行事件里，形如 `glm -> mm (billing: ...)`。

跨源的 `previous_response_id` 续接会被自动丢弃并改发完整历史——续接 ID 是厂商私有的，不能跨源复用。

### 3.1 频繁 429 怎么办：并发自适应

429 在订阅套餐里几乎总是"并发太多"，不是"额度用完"——GLM 的 Lite/Pro/Max 分别只建议 1 / 1–2 / 2+ 个并发项目，而本项目的团队默认 4 侦察 + 12 审计，峰值并发能到 12 以上。所以池内每个源都带一个自适应限流器（AIMD）：

- **429 → 该源并发减半**（4→2→1），并记录 `retry-after`；另一侧有余量就先用另一侧，没有就退避后重试同一源，**不再整队暂停**；
- **连续成功 → 每 20 次 +1**，逐步试探回上限；刚被限流后有 2 分钟冷却期，避免抖动；
- **并发已经降到 1 还在 429，或连续 8 次 429** → 这才判定是套餐额度问题，转入暂停/换源；
- 上限默认等于团队规模（`recon + audit + 1`），起步 4，可在 `concurrency` 段覆盖。

```yaml
concurrency:
  max_in_flight: 0          # 0 = 团队规模
  initial_in_flight: 4
  min_in_flight: 1
  increase_after_successes: 20
  decrease_factor: 0.5
```

限的是**在飞请求数**，不是 Agent 进程数：Agent 在等槽位时上下文完好，不丢工作。变化会以事件形式出现在运行日志里（`glm 并发 4 → 2（429 限流，降低并发）`），`/providers` 与 `-check` 也能看到 `并发 x/y · 在飞 n · 429 次数`。

### 4. 工作时间段

```yaml
schedule:
  enabled: true
  timezone: Asia/Shanghai
  windows:
    - {start: "21:00", end: "08:00"}    # 跨午夜
  on_window_close: pause                # 或 finish_current_task
  poll_seconds: 30
```

窗口外暂停并保存进度，到点自动 `继续基于当前团队状态`。跨午夜按"窗口开启那天"归属，所以周五 22:00–周六 06:00 只属于周五。

可以配**多个窗口**、每个窗口**按周几生效**；也可以在 TUI 里改，改动写进 `schedule.store_file`（默认 `state/schedule.json`）并**覆盖配置文件**，重启后仍是你最后设置的那套：

```text
/window                          # 打开编辑器：a 新增、Enter/e 编辑、d 删除、空格 开关
/window 22:00-06:00 mon-fri      # 命令行式一条搞定
/window 00:00-23:59 weekend      # 周末全天
/window off                      # 关闭时段限制
```

编辑器里的"星期"字段接受 `all` / `weekdays` / `weekend` / `mon-fri` / `mon,wed,fri`。删除最后一个窗口会自动关闭时段限制，避免"永远暂停"这种隐形坑。

#### 渠道级时段（局部覆盖全局）

时段是两层：全局一套，渠道可以各自覆盖，例如"夜里跑 MiniMax，GLM 全天可用"：

```yaml
schedule:                 # 全局：只在夜间
  enabled: true
  windows: [{start: "21:00", end: "08:00"}]

providers:
  - id: minimax           # 不写 schedule → 跟随全局（只在夜间）
  - id: glm
    schedule:
      enabled: false      # 覆盖：本渠道忽略全局，全天可用
```

命令行同样能改：

```text
/window glm always            渠道全天（忽略全局）
/window glm inherit           恢复跟随全局
/window glm 21:00-02:00 all   渠道自己的窗口
```

判定顺序：**渠道自己的策略优先，其次全局**。只要还有任一渠道处于开窗状态，程序就不会整体暂停；路由器也只会挑当前开窗的渠道（时段外的渠道即使余量充足也不会被调用），所以"夜里的 MiniMax + 白天的 GLM"是自然结果。这些覆盖同样写进 `state/schedule.json`，重启保留。

### 5. 任务编排

```yaml
# tasks.yaml
tasks:
  - id: T001
    title: 智通人才
    dir: /path/to/repo
    prompt: 先看鉴权与上传
    budget: {hours: 3}
```

```text
/tasks import tasks.yaml          # 也支持 tasks.txt（每行 "目录 | 提示词"）
/tasks import /path/to/repos --each-subdir   # 每个子目录一个任务
/tasks                            # 查看队列
/tasks run                        # 依次串行执行，无人值守
/tasks skip T002 · /tasks retry T003
```

每任务独立：`sessions/tasks/T001.json`（可断点续跑）、`reports/T001/report.json` 与 `report.md`。进程被杀后重新打开，队列从游标继续。中途余量耗尽 → 任务标记 `已暂停`、**不消耗重试次数**、会话保存，余量恢复后原地续跑。

### 6. 命令速查

| 命令 | 作用 |
| --- | --- |
| `/providers` | 渠道列表（vendor/plan、余量、并发、429 次数）；↑↓ 选择、Enter 强制切换、e 启停、r 刷新余量 |
| `/providers <ID>` | 直接切到某个源 |
| `/quota [ID]` | 余量与恢复时间；带 ID 时刷新并显示原始响应 |
| `/window` | 时段编辑器（多窗口、按周几、持久化）；`/window off|on|22:00-06:00 mon-fri` 直改 |
| `/limit` | 自设用量上限：`/limit glm 90%`、`reserve 500`、`off` |
| `/tasks ...` | 导入 / 运行 / 停止 / 跳过 / 重试任务队列 |
| `/window <渠道> ...` | 渠道级时段：`glm always` / `glm inherit` / `glm 21:00-02:00 mon-fri` |
| `-check` | 非交互自检：源、余量（含接口报告的档位）、时段状态、任务队列 |

> 说明：GLM Coding Plan 的条款限定"仅在官方指定工具中使用"，自建程序调用可能触发限流或权益限制。程序把每个源都当作普通 OpenAI 兼容端点，随时可以换成标准 API key（改 `base_url`/`api_key_env`，不动代码）。

### 7. 运维细节

**状态文件**（全部会被 gitignore，权限 0600，可随时删除重建）：

| 文件 | 内容 | 删掉的后果 |
| --- | --- | --- |
| `state/quota.json` | 各渠道 5 小时窗口的消耗与重置时间 | 重启会把半个窗口当成满额，可能短暂超用 |
| `state/limits.json` | `/limit` 设的自设用量上限 | 回到配置文件里的值 |
| `state/schedule.json` | `/window` 改的全局时段与渠道覆盖 | 回到配置文件里的时段 |
| `sessions/tasks/<ID>.json` | 任务断点 | 该任务从头开始 |
| `reports/<ID>/report.{json,md}` | 每任务产出 | 需重新导出 |

**几个容易踩的点**：

- **压缩预留要按最紧的源算**：多源时压缩阈值取所有启用源里最小的 `context / output`。GLM 是 200k 上下文，所以 `agent.compress_buffer_tokens` 不能沿用单源 1M 时的 393216，否则配置加载直接报 "openai context cannot fit output plus reserve"。当前配置用 32768。
- **没填 key 的渠道会被跳过**，不会让程序起不来；填好后重启即生效。
- **429 的处理顺序**是：降并发 → 换源 → 退避重试 → 并发到底仍 429 才判定额度问题并暂停。所以看到"变慢"通常是在降并发，不是卡住。
- **额度与时段是两套刹车**：余量不够会换源/暂停；时段之外直接不选该渠道。两者都会写入运行事件，便于回溯。
- **敏感信息只走 env**：`api_key_env` + `.env`（`run.sh` 会自动 source），不要写进 `config.yaml` 提交。

## 技术报告
具体技术细节和技术报告,我会在[key08.com](https://key08.com/)发布,尽请关注.
