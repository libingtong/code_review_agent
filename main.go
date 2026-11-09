package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/applog"
	"code-review-agent/internal/config"
	"code-review-agent/internal/orchestrator"
	"code-review-agent/internal/paniclog"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/provider"
	"code-review-agent/internal/report"
	"code-review-agent/internal/schedule"
	"code-review-agent/internal/tools"
	"code-review-agent/internal/tui"
)

// buildVersion is injected by run.sh (-ldflags "-X main.buildVersion=...").
var buildVersion = "dev"

func main() {
	defer paniclog.RecoverWithContext("main")

	cfgPath := flag.String("config", "config.yaml", "config file path")
	auditDir := flag.String("dir", "", "directory to audit")
	check := flag.Bool("check", false, "print provider/quota/schedule/queue status and exit")
	headless := flag.Bool("run", false, "headless: audit -dir, write session/report, then exit")
	maxMinutes := flag.Int("max-minutes", 0, "with -run: stop after N minutes (0 = run until interrupted)")
	resume := flag.String("resume", "", "with -run: continue from a saved session (path, or latest for the newest)")
	rescan := flag.Bool("rescan", false, "with -resume: reuse the saved reconnaissance map and start a fresh audit stage")
	keepBudget := flag.Bool("keep-budget", false, "with -resume: keep the saved elapsed/token usage instead of a fresh window")
	version := flag.Bool("version", false, "print build version and exit")
	flag.Parse()
	if *version {
		fmt.Printf("code-review-agent %s\n", buildVersion)
		return
	}

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

	if *headless {
		os.Exit(runHeadless(cfg, runner, gate, *auditDir, headlessOptions{maxMinutes: *maxMinutes, resume: *resume, rescan: *rescan, keepBudget: *keepBudget}))
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

// runHeadless audits without the TUI: the same team, the same session/report
// artifacts, but events go to stderr. It exists so a scan can be started from a
// script (or by an agent) and its log inspected afterwards.
// headlessOptions carries the run knobs so the signature stays readable.
type headlessOptions struct {
	maxMinutes int
	resume     string
	rescan     bool
	keepBudget bool
}

const maxAutomaticResumeSessionBytes = 128 << 20

// resolveSession turns "latest" (or an empty latest-ish selector) into the newest
// session file, so resuming does not require typing a timestamp.
func resolveSession(selector string) (string, error) {
	if selector != "latest" {
		return selector, nil
	}
	entries, err := filepath.Glob(filepath.Join("sessions", "*.json"))
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("sessions/ 下没有可恢复的会话")
	}
	var newest string
	var newestInfo os.FileInfo
	for _, entry := range entries {
		info, err := os.Stat(entry)
		if err != nil || info.Size() > maxAutomaticResumeSessionBytes {
			continue
		}
		if newest == "" || info.ModTime().After(newestInfo.ModTime()) {
			newest, newestInfo = entry, info
		}
	}
	if newest == "" {
		return "", fmt.Errorf("sessions/ 下没有可自动恢复的会话（已跳过超过 %d MB 的异常大文件）", maxAutomaticResumeSessionBytes>>20)
	}
	return newest, nil
}

func runHeadless(cfg config.Config, team *agent.Team, gate *schedule.Gate, dir string, opts headlessOptions) int {
	maxMinutes := opts.maxMinutes
	if strings.TrimSpace(opts.resume) != "" {
		// A resumed session carries its own workspace, so -dir is optional here.
		path, err := resolveSession(opts.resume)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：%v\n", err)
			return 2
		}
		if err := team.LoadSession(path); err != nil {
			fmt.Fprintf(os.Stderr, "错误：恢复会话失败：%v\n", err)
			return 2
		}
		if opts.rescan {
			if err := team.ReuseRecon(); err != nil {
				fmt.Fprintf(os.Stderr, "错误：复用侦察失败：%v\n", err)
				return 2
			}
		}
		if !opts.keepBudget {
			team.ResetBudgetUsage()
		}
		dir = team.Workspace()
		snapshot := team.Snapshot()
		coverage := tools.ComputeCoverage(snapshot)
		mode := ""
		if opts.rescan {
			mode = " · 仅复用侦察，重建审计团队"
		}
		fmt.Fprintf(os.Stderr, "恢复会话：%s · 阶段 %s · 范围 %d 个文件（已审 %d）· 漏洞 %d%s\n", path, team.Phase(), coverage.Total, coverage.Reviewed, len(snapshot.Findings), mode)
	}
	if strings.TrimSpace(dir) == "" {
		dir = cfg.Workspace
	}
	if strings.TrimSpace(dir) == "" {
		fmt.Fprintf(os.Stderr, "错误：需要 -dir <目录>（或用 -run 时指定工作区）\n")
		return 2
	}
	dir = strings.TrimSuffix(filepath.Clean(dir), "/")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "错误：目录不存在或不可读：%s\n", dir)
		return 2
	}
	if decision := gate.Check(); !decision.Allowed {
		wait := time.Until(decision.ResumeAt)
		if wait <= 0 || wait > 30*time.Minute {
			fmt.Fprintf(os.Stderr, "当前不允许执行：%s（预计 %s 恢复）\n", decision.Reason, decision.ResumeAt.Local().Format("01-02 15:04"))
			return 3
		}
		fmt.Fprintf(os.Stderr, "工作时段未开启（%s），等待 %s 后开始…\n", decision.Reason, wait.Round(time.Minute))
		time.Sleep(wait)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if maxMinutes > 0 {
		// Infinite mode has no end_audit tool, so a headless scan needs its own cap;
		// without it the run only stops when someone interrupts it.
		var stopDeadline context.CancelFunc
		ctx, stopDeadline = context.WithTimeout(ctx, time.Duration(maxMinutes)*time.Minute)
		defer stopDeadline()
		fmt.Fprintf(os.Stderr, "时间上限：%d 分钟\n", maxMinutes)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		fmt.Fprintln(os.Stderr, "\n收到中断，正在保存并停止…")
		cancel()
	}()

	// Track the latest state so a compact progress line can be printed.
	var lastState agent.Event
	emit := func(event agent.Event) {
		switch event.Kind {
		case "tool":
			fmt.Fprintf(os.Stderr, "[%s] %s\n", event.AgentID, event.Content)
		case "error":
			fmt.Fprintf(os.Stderr, "[%s] 错误：%s\n", event.AgentID, event.Content)
		case "state":
			lastState = event
			if len(event.Workers) > 0 {
				// Progress first: how much of the workspace is audited, what is left,
				// and how long that should take at the observed rate.
				coverage := tools.ComputeCoverage(team.Snapshot())
				fmt.Fprintf(os.Stderr, "· %s | %s | 成员 %d\n",
					progressLine(event),
					coverage.String(team.BudgetStatus().Elapsed),
					len(event.Workers))
			}
		}
	}

	budget := team.BudgetStatus()
	limit := budgetLabel(budget)
	if budget.TokenLimit > 0 {
		limit += fmt.Sprintf(" · token 上限 %d", budget.TokenLimit)
	}
	fmt.Fprintf(os.Stderr, "开始审计：%s（预算 %s · 已用 %d tokens）\n", dir, limit, budget.UsedTokens)
	start := time.Now()
	team.Run(ctx, dir, emit)
	elapsed := time.Since(start).Round(time.Second)

	sessionPath := filepath.Join("sessions", time.Now().Format("20060102-150405")+".json")
	if err := os.MkdirAll("sessions", 0o700); err == nil {
		if err := team.SaveSession(sessionPath); err != nil {
			fmt.Fprintf(os.Stderr, "警告：保存会话失败：%v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "会话：%s\n", sessionPath)
		}
	}

	snapshot := team.Snapshot()
	built := report.Build(snapshot, team.Revocations())
	reportDir := filepath.Join("reports", "headless-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(reportDir, 0o700); err == nil {
		if err := report.WriteJSON(filepath.Join(reportDir, "report.json"), built); err == nil {
			_ = report.WriteMarkdown(filepath.Join(reportDir, "report.md"), built)
			fmt.Fprintf(os.Stderr, "报告：%s\n", filepath.Join(reportDir, "report.md"))
		} else {
			fmt.Fprintf(os.Stderr, "警告：写报告失败：%v\n", err)
		}
	}
	_ = lastState

	files := 0
	for _, entry := range snapshot.Files {
		switch entry.Status {
		case "reviewed", "done", "complete":
			files++
		}
	}
	fmt.Fprintf(os.Stderr, "完成：用时 %s · 文件 %d/%d · 漏洞 %d\n", elapsed, files, len(snapshot.Files), len(snapshot.Findings))
	if ctx.Err() != nil {
		return 130
	}
	if len(snapshot.Findings) == 0 && files == 0 {
		return 1
	}
	return 0
}

// budgetLabel summarises the team budget for the headless header line.
func budgetLabel(status agent.BudgetStatus) string {
	if status.InfiniteMode {
		return "无限"
	}
	return fmt.Sprintf("%dh%02dm", status.Hours, status.Minutes)
}

// progressLine renders a one-line progress summary for headless runs.
func progressLine(event agent.Event) string {
	parts := make([]string, 0, len(event.Workers))
	for _, worker := range event.Workers {
		parts = append(parts, fmt.Sprintf("%s=%s/%d", worker.ID, worker.Status, worker.Turn))
	}
	return strings.Join(parts, " ")
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
