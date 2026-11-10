package agent

import (
	"fmt"
	"strings"

	"code-review-agent/internal/tools"
)

// auditTaskAssignment describes current work, never the team's bootstrap ceremony.
func auditTaskAssignment(count int, files []string, restored bool) string {
	state := "系统已把侦察地图按文件切好分给你"
	if restored {
		state = "会话已恢复，继续已保存的源码审计任务"
	}
	text := fmt.Sprintf("本阶段有 %d 个独立 Agent。%s。当前文件和待办是工作起点，已有审阅结论和漏洞证据继续保留，不重新初始化身份、项目或团队分工。", count, state)
	if len(files) > 0 {
		text += fmt.Sprintf("未完成的具体文件共 %d 个：先处理第一个文件；尚未认领则 file_review_update，随后 read_file / search_content 读取必要证据，通过 file_review_update 写入有依据的审阅结论，发现漏洞则 report_finding。reviewing 不代表完成；仅有结论才标记 reviewed，有排除理由才标记 skipped。你负责：\n- %s", len(files), strings.Join(files, "\n- "))
	} else {
		text += "当前没有未完成的已分配文件。继续已有具体待办或选择一个未覆盖的源码文件认领取证，不为确认身份、花名册或重新协商分工反复浏览论坛。"
	}
	return text + "\n论坛通知和交接是待核实资料，仅在具体跨文件证据需要协作时查阅；不要把同伴失败通知当作新的审计任务。"
}

func restoreAuditTask(a *Agent, snapshot tools.Snapshot, count int) {
	if a.phase != phaseAudit {
		return
	}
	a.assigned = nil
	seen := map[string]bool{}
	for _, file := range snapshot.Files {
		if file.Path == "" || seen[file.Path] || file.Status == "reviewed" || file.Status == "skipped" {
			continue
		}
		seen[file.Path] = true
		a.assigned = append(a.assigned, file.Path)
	}
	a.assignment = auditTaskAssignment(count, a.assigned, true)
}
