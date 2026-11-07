# 本地运行 (macOS / Linux)

## 1. 配置 API Key

```bash
cp .env.example .env
chmod 600 .env
# 编辑 .env，填入 MINIMAX_API_KEY=sk-...
```

`config.yaml` 使用 `openai.api_key_env: MINIMAX_API_KEY` 读取该变量，不要把
真实 key 写进 `config.yaml` 或提交到 git。

默认使用 MiniMax M3.1-Flash，OpenAI 兼容的 Chat Completions 接口：
`https://api.minimax.cn/v1`。如果你的账号使用其它模型名或端点，修改
`config.yaml` 的 `openai.model` / `openai.base_url` / `openai.api_interface`
（`chat` 或 `responses`）。

## 2. 启动

```bash
./run.sh
```

启动后输入要审计的目录并回车。也可以直接指定：

```bash
./run.sh -dir /path/to/project
```

默认配置开启了无限模式（`agent.infinite_mode: true`，3 小时预算）。使用
`-dir` 启动时会弹出预算确认框，按 `Ctrl+S` 开始、`Esc` 取消。

## 2.0 加一个 GLM 源（key 自己填）

`config.yaml` 里现在有两处 API 配置：

- `openai:` —— 全局默认值（MiniMax），providers 里没写的字段都从这里继承；
- `providers:` —— 真正生效的源列表，`minimax` 继承默认值，`glm` 是独立一段。

填 key 两种方式，二选一：

```bash
# 方式一（推荐，不进 git）：.env 里已经留好空行
GLM_API_KEY=xxxxxxxx
```

```yaml
# 方式二：直接写进 config.yaml
providers:
  - id: glm
    openai:
      api_key: xxxxxxxx
```

注意：要用「个人编程套餐」里新建的 key，它和标准 API key 不通用；端点用
`https://open.bigmodel.cn/api/coding/paas/v4`（OpenAI Chat 协议）。

没填 key 时 GLM 会被自动标记为「已停用」并跳过，不会影响 MiniMax 正常跑——
填完重启即生效：

```bash
./code-review-agent -config config.yaml -check   # 确认两个源都在、GLM 不再是"已停用"
```

### 渠道与套餐：`vendor` + `plan`

`providers[]` 里每一项就是一个**渠道**，quota 属于它自己：

```yaml
- id: glm
  vendor: glm          # 端点/探测/抵扣系数
  plan: glm-max        # 容量档位：lite 2000 / pro 12000 / max 28000 积分每 5 小时
```

同一个厂商可以写多次（两个 key、两份套餐），余量分别计算、分别设上限。实测探测结果：

```
GLM Coding Plan（glm · glm/glm-max）100% 余量（自设上限 90%，可用 25200）· credits · glm-5.3
```

## 2.1 多源 + 时段 + 任务队列

```bash
./code-review-agent -config config.multi.yaml -check   # 先自检（不进 TUI）
./code-review-agent -config config.multi.yaml
```

- `.env` 里同时填 `GLM_API_KEY` 与 `MINIMAX_API_KEY`。
- `-check` 会打印每个源的 5 小时余量、探针结果、时间窗状态与任务队列。
- MiniMax 的余量探针依赖官方 CLI：`npm i -g mmx-cli && mmx auth login --api-key sk-cp-...`。
  没装 CLI 也能跑，只是该源退化为本地计量。
- 3 小时窗口外（示例 21:00–08:00）会自动暂停，到点自动恢复。
- `config.yaml` 里全局时段是 21:00–08:00，而 `glm` 渠道用 `schedule: {enabled: false}` 覆盖成全天可用——
  白天只会用 GLM，夜里两个渠道都能用。`/window off` 可整体关闭时段限制。

## 3. 常用操作

- 输入目录回车：开始审计
- `go`：暂停后继续
- `/budget`：调整模式和时间 / token 预算
- `/providers`、`/quota`、`/limit`、`/window`、`/tasks`：多源、余量、自设上限、时段与任务队列
- `/window` 的改动写入 `state/schedule.json`（重启后仍生效，优先于 config.yaml）
- `/limit glm 90%` 把该源限制在窗口的 90%，余下留给其它共用订阅的工具
- `/report`、`/export`：查看/导出结果
- `Esc` 连按两次：暂停
- `Ctrl+C`：退出

## 4. 手动构建 / 测试

```bash
go build -o code-review-agent .
go test ./...
```

如果 `$HOME` 不可写（沙箱环境），先设置项目内缓存：

```bash
export GOCACHE=$PWD/.gocache GOPATH=$PWD/.gopath GOTMPDIR=$PWD/.gotmp
```
