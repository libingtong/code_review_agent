package tools

import (
	"path"
	"sort"
	"strings"
)

// auditSourceExts are the file types that can actually hold a defect. Everything
// else (docs, images, lock files) would only pad the map.
var auditSourceExts = map[string]bool{
	".java": true, ".kt": true, ".scala": true, ".go": true, ".py": true,
	".js": true, ".ts": true, ".jsx": true, ".tsx": true, ".vue": true,
	".php": true, ".rb": true, ".cs": true, ".c": true, ".cc": true, ".cpp": true,
	".h": true, ".hpp": true, ".rs": true, ".lua": true, ".pl": true, ".swift": true,
	".xml": true, ".jsp": true, ".ftl": true, ".vm": true, ".html": true,
	".yml": true, ".yaml": true, ".properties": true, ".conf": true, ".ini": true,
	".sql": true, ".sh": true,
}

// auditWeakDirSegments mark paths that never deserve a map slot.
var auditWeakDirSegments = []string{
	"/test/", "/tests/", "/docs/", "/doc/", "/example/", "/examples/",
	"/fixture/", "/fixtures/", "/testdata/", "/vendor/", "/node_modules/",
	"/static/", "/assets/", "/dist/", "/build/",
}

type auditWeight struct {
	score int
	words []string
}

// auditWeights is ordered by signal strength. A controller or auth filter catches
// both access-control and injection defects, so it outranks a helper class.
var auditWeights = []auditWeight{
	{100, []string{"controller", "handler", "/action/", "endpoint", "servlet", "/api/"}},
	{96, []string{"filter", "/security", "shiro", "springsecurity", "permission", "authorize", "authorization", "principal"}},
	{94, []string{"login", "logout", "session", "token", "jwt", "auth", "sso", "oauth"}},
	{92, []string{"upload", "storage", "oss", "minio", "/s3", "download", "attachment"}},
	{90, []string{"callback", "notify", "pay", "wechat", "weixin", "alipay", "sms", "thirdpart", "webhook"}},
	{86, []string{"dao", "mapper", "repository", "jdbc", "hibernate", "mybatis", "sql"}},
	{82, []string{"encrypt", "decrypt", "crypto", "cipher", "aes", "rsa", "secret", "keymanager", "password", "captcha", "kaptcha"}},
	{74, []string{"service", "manager", "biz", "business", "facade", "provider"}},
	{60, []string{"interceptor", "aspect", "aop", "middleware", "config", "properties", "settings", "application"}},
	{50, []string{"template", "jsp", "ftl", "vm", "theme", "view", "front"}},
}

// auditPriority scores one inventory path. 0 means "never seed this file".
// The scoring is deterministic on purpose: a thin model map used to collapse the
// whole audit to zero findings, so the scope needs a model-independent floor.
func auditPriority(entry InventoryEntry) int {
	if entry.LowValue {
		return 0
	}
	lower := strings.ToLower(entry.Path)
	ext := entry.Ext
	if ext == "" {
		ext = strings.ToLower(path.Ext(lower))
	}
	if !auditSourceExts[ext] {
		return 0
	}
	padded := "/" + lower
	for _, weak := range auditWeakDirSegments {
		if strings.Contains(padded, weak) {
			return 0
		}
	}
	score := 12
	for _, weight := range auditWeights {
		for _, word := range weight.words {
			if strings.Contains(lower, word) && weight.score > score {
				score = weight.score
				break
			}
		}
	}
	// Application code under src/main beats loose scripts at the repository root.
	if strings.Contains(padded, "/src/main/") || strings.Contains(padded, "/src/") {
		score += 3
	}
	return score
}

// SuggestAuditFiles returns the highest-signal files the audit scope should hold,
// at most perDir files from one directory so a single package cannot eat the quota.
func (r *Registry) SuggestAuditFiles(limit int, perDir int) []string {
	if limit <= 0 {
		return nil
	}
	if perDir <= 0 {
		perDir = 4
	}
	type candidate struct {
		path  string
		score int
		depth int
	}
	var candidates []candidate
	for _, entry := range r.inventory {
		score := auditPriority(entry)
		if score == 0 {
			continue
		}
		candidates = append(candidates, candidate{path: entry.Path, score: score, depth: strings.Count(entry.Path, "/")})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth < candidates[j].depth
		}
		return candidates[i].path < candidates[j].path
	})
	used := map[string]int{}
	out := make([]string, 0, limit)
	for _, candidate := range candidates {
		dir := path.Dir(candidate.path)
		if used[dir] >= perDir {
			continue
		}
		used[dir]++
		out = append(out, candidate.path)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// PlanAuditScope registers the files a plan intends to audit. They are "pending",
// not "reviewing": nobody has claimed them yet, and coverage must not report them
// as work in progress just because they appear in a map.
func (r *Registry) PlanAuditScope(paths []string) []string {
	var added []string
	for _, p := range paths {
		p = normalizeInventoryPath(p)
		if p == "" || !r.inventoryHasPath(p) {
			continue
		}
		found := false
		for i := range r.files {
			if r.files[i].Path != p {
				continue
			}
			found = true
			if r.files[i].Status == "skipped" {
				r.files[i].Status = "pending"
			}
			if r.files[i].Note == "" {
				r.files[i].Note = "侦察地图计划审计范围"
			}
			break
		}
		if !found {
			r.files = append(r.files, FileReview{Path: p, Status: "pending", Note: "侦察地图计划审计范围"})
		}
		added = append(added, p)
	}
	return added
}

// SeedAuditScope adds system-picked files to the review scope and marks where they
// came from, so the report can separate model picks from the deterministic floor.
func (r *Registry) SeedAuditScope(paths []string) []string {
	var added []string
	for _, p := range paths {
		p = normalizeInventoryPath(p)
		if p == "" || !r.inventoryHasPath(p) {
			continue
		}
		found := false
		for i := range r.files {
			if r.files[i].Path != p {
				continue
			}
			found = true
			if r.files[i].Status == "skipped" {
				r.files[i].Status = "reviewing"
			}
			if r.files[i].Note == "" {
				r.files[i].Note = "系统按风险关键词补入审计范围"
			}
			break
		}
		if !found {
			r.files = append(r.files, FileReview{Path: p, Status: "reviewing", Note: "系统按风险关键词补入审计范围"})
		}
		added = append(added, p)
	}
	return added
}
