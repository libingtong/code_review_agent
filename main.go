package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"code-review-agent/internal/agent"
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
