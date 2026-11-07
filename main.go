package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/applog"
	"code-review-agent/internal/config"
	"code-review-agent/internal/orchestrator"
	"code-review-agent/internal/paniclog"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/provider"
	"code-review-agent/internal/schedule"
	"code-review-agent/internal/tools"
	"code-review-agent/internal/tui"
)

func main() {
	defer paniclog.RecoverWithContext("main")

	cfgPath := flag.String("config", "config.yaml", "config file path")
	auditDir := flag.String("dir", "", "directory to audit")
	check := flag.Bool("check", false, "print provider/quota/schedule/queue status and exit")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if *auditDir != "" {
		cfg.Workspace = *auditDir
	}

	logger, err := applog.Open(cfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: 无法开启日志：%v\n", err)
	}
	applog.SetLogger(logger)
	defer func() {
		if logger != nil {
			_ = logger.Close()
		}
	}()
	if logger != nil {
		logger.Infof("启动：config=%s workspace=%s", *cfgPath, cfg.Workspace)
		for _, provider := range cfg.Providers {
			key := "missing"
			if provider.OpenAI.APIKey != "" {
				key = "set"
			}
			logger.Infof("渠道 %s vendor=%s plan=%s model=%s base=%s interface=%s key=%s enabled=%v",
				provider.ID, provider.VendorOf(), provider.Plan, provider.OpenAI.Model,
				provider.OpenAI.BaseURL, provider.OpenAI.APIInterface, key, provider.IsEnabled())
		}
		fmt.Fprintf(os.Stderr, "日志：%s\n", logger.Path())
	}

	missing := cfg.MissingKeys()
	enabled := 0
	for _, provider := range cfg.Providers {
		if provider.IsEnabled() {
			enabled++
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "警告：以下渠道没有可用的 API key，将被跳过：%s\n", strings.Join(missing, "、"))
	}
	if enabled > 0 && len(missing) == enabled {
		fmt.Fprintf(os.Stderr, "错误：所有启用渠道都缺少 API key。请在 .env 中配置后重试（例如 GLM_API_KEY / MINIMAX_API_KEY），或用 ./run.sh 启动。\n")
		os.Exit(1)
	}

	prompts, err := prompt.Load(cfg.Prompts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load prompts: %v\n", err)
		os.Exit(1)
	}

	pool, err := provider.NewPool(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init providers: %v\n", err)
		os.Exit(1)
	}
	pool.SetQuotaStatePath(cfg.Quota.StateFile)
	pool.SetLimitsPath(cfg.Quota.LimitsFile)
	if err := pool.LoadQuotaState(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: 无法恢复余量状态（将从零计量）：%v\n", err)
	}
	if err := pool.LoadLimits(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: 无法恢复自设用量上限：%v\n", err)
	}
	pool.SetSelectionPath(cfg.Routing.SelectionFile)
	if err := pool.LoadSelection(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: 无法恢复上次选择的渠道/模型：%v\n", err)
	}
	pool.SetConcurrencyStatePath(cfg.Concurrency.StateFile)
	if err := pool.LoadConcurrencyState(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: 无法恢复并发学习值：%v\n", err)
	}
	quotaCtx, stopQuota := context.WithCancel(context.Background())
	defer stopQuota()
	go pool.StartQuotaLoop(quotaCtx)

	registry, err := tools.NewRegistry(cfg.Workspace, cfg.Agent.MaxToolResultChars)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init tools: %v\n", err)
		os.Exit(1)
	}

	// Two-level policy: a global window plus per-channel overrides. Runtime
	// /window edits live in their own file and win over the config file, so a
	// restart keeps whatever the user last set in the UI.
	plan, err := schedule.FromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load schedule: %v\n", err)
		os.Exit(1)
	}
	gate := &schedule.Gate{Quota: pool}
	gate.SetPlan(plan)
	pool.SetChannelClock(plan)

	runner := agent.NewTeam(cfg, prompts, pool, pool, registry)
	runner.SetPool(pool)
	runner.SetGate(gateAdapter{gate: gate})
	defer runner.Close()

	var tasks *orchestrator.Runner
	if cfg.Orchestrator.Enabled {
		tasks, err = orchestrator.NewRunner(runner, cfg.Orchestrator, gate, cfg.Orchestrator.StateFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: 任务编排不可用：%v\n", err)
		}
	}

	if *check {
		printCheck(cfg, pool, gate, tasks)
		return
	}

	view := tui.New(runner, cfg, *auditDir)
	view.AttachProviders(pool, gate, tasks, func() {
		_, _, _ = pool.RefreshQuota(context.Background(), "")
	})
	program := tea.NewProgram(view, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "run tui: %v\n", err)
		os.Exit(1)
	}
}

// printCheck is the non-interactive smoke path: it reports exactly what the TUI
// would show, so configuration and probes can be verified without a terminal.
func printCheck(cfg config.Config, pool *provider.Pool, gate *schedule.Gate, tasks *orchestrator.Runner) {
	fmt.Printf("工作区：%s\n", cfg.Workspace)
	fmt.Println("源：")
	for _, line := range pool.QuotaLines() {
		fmt.Println("  " + line)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, status := range pool.Statuses() {
		sample, raw, err := pool.RefreshQuota(probeCtx, status.ID)
		if err != nil {
			fmt.Printf("  探针 %s：%v\n", status.ID, err)
			// Still report the model list: a missing model is a different problem
			// from a missing quota probe.
			printModelCheck(probeCtx, pool, status)
			continue
		}
		plan := ""
		if sample.Plan != "" {
			plan = " plan=" + sample.Plan
		}
		reset := "—"
		if !sample.ResetAt.IsZero() {
			reset = sample.ResetAt.Local().Format(time.RFC3339)
		}
		fmt.Printf("  探针 %s：used=%s total=%s%s reset=%s raw=%d bytes\n",
			status.ID, provider.FormatUnits(sample.Used), provider.FormatUnits(sample.Total), plan, reset, len(raw))
		printModelCheck(probeCtx, pool, status)
	}
	fmt.Printf("时间窗：%s\n", gate.Describe())
	decision := gate.Check()
	if decision.Allowed {
		fmt.Println("当前允许执行")
	} else {
		fmt.Printf("当前暂停：%s（预计 %s 恢复）\n", decision.Reason, decision.ResumeAt.Local().Format(time.RFC3339))
	}
	if tasks != nil {
		fmt.Printf("任务队列：%s\n", tasks.Summary())
		for _, task := range tasks.Tasks() {
			fmt.Printf("  %s [%s] %s\n", task.ID, task.Status, task.Title)
		}
	}
}

// printModelCheck reports whether the configured model exists on that endpoint.
func printModelCheck(ctx context.Context, pool *provider.Pool, status provider.Status) {
	if !status.Enabled {
		return
	}
	toolOK, preview, toolErr := pool.ProbeToolSupport(ctx, status.ID)
	switch {
	case toolErr != nil:
		fmt.Printf("  工具 %s：探测失败（%v）\n", status.Model, toolErr)
	case toolOK:
		fmt.Printf("  工具 %s：原生 function_call OK\n", status.Model)
	default:
		hint := ""
		if preview != "" {
			hint = "；模型回答开头：" + preview
		}
		fmt.Printf("  工具 %s：未返回 function_call ← 该模型在这个端点上可能不支持工具调用%s\n", status.Model, hint)
	}
	models, err := pool.ListModels(ctx, status.ID)
	if err != nil {
		fmt.Printf("  模型 %s：端点未提供列表（%v）\n", status.Model, err)
		return
	}
	found := false
	for _, name := range models {
		if name == status.Model {
			found = true
			break
		}
	}
	note := ""
	if !found {
		note = "  ← 当前配置的名字不在列表中，请改成下列之一"
	}
	fmt.Printf("  模型 %s：可用 %s%s\n", status.Model, strings.Join(shortList(models, 12), ", "), note)
}

// shortList bounds a model list for terminal output.
func shortList(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	out := append([]string(nil), values[:limit]...)
	return append(out, fmt.Sprintf("…共 %d 个", len(values)))
}

// gateAdapter exposes schedule.Gate to the agent layer without leaking its
// decision type across packages.
type gateAdapter struct{ gate *schedule.Gate }

func (a gateAdapter) Check() agent.GateDecision {
	decision := a.gate.Check()
	return agent.GateDecision{Allowed: decision.Allowed, Reason: decision.Reason, ResumeAt: decision.ResumeAt}
}
