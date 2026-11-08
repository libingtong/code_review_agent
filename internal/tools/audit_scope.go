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

// auditBucket is one class of high-risk code. Buckets are *interleaved* when the
// scope is built: additive keyword scoring let "…SessionDao" files outrank the
// filter and crypto classes, so the plan never reached XssFilter or HybridEncryption
// even though those are exactly the control-plane files to read first.
type auditBucket struct {
	name    string
	words   []string
	primary []string
	// nameOnly files must match in the file name, not just the package.
	nameOnly bool
}

// guaranteedPerBucket is how many files each risk class contributes before the
// rotation fills the rest.
const guaranteedPerBucket = 5

var auditBuckets = []auditBucket{
	// primary = the specific, high-risk variant of a class; generic = the class word.
	// "OSSController" (primary oss) must outrank "UploadFileController" (generic ×2).
	{name: "crypto", words: []string{"encrypt", "decrypt", "cipher", "aes", "rsa", "secret", "keymanager", "password", "captcha", "kaptcha"}, primary: []string{"encrypt", "decrypt", "cipher", "rsa", "secret", "keymanager", "password", "captcha"}},
	{name: "upload", words: []string{"upload", "oss", "minio", "storage", "download", "attachment"}, primary: []string{"oss", "minio", "storage", "download", "attachment"}},
	{name: "callback", words: []string{"callback", "notify", "pay", "wechat", "weixin", "alipay", "sms", "webhook", "thirdpart"}, primary: []string{"callback", "notify", "alipay", "webhook", "thirdpart"}},
	{name: "filter", words: []string{"filter", "interceptor", "wrapper", "xss", "csrf", "aspect", "aop", "middleware"}, primary: []string{"xss", "csrf", "filter", "interceptor"}},
	{name: "auth", words: []string{"auth", "login", "logout", "session", "token", "jwt", "sso", "oauth", "permission", "principal"}, primary: []string{"auth", "jwt", "oauth", "sso", "permission"}},
	{name: "dao", words: []string{"dao", "mapper", "repository", "jdbc", "hibernate", "mybatis", "recommend", "query", "where"}, primary: []string{"mapper", "jdbc", "mybatis", "recommend", "query", "where"}},
	{name: "service", words: []string{"service", "manager", "biz", "business", "facade", "provider"}, primary: []string{}},
	{name: "endpoint", words: []string{"controller", "handler", "action", "endpoint", "servlet"}, primary: []string{}},
	{name: "config", words: []string{"config", "properties", "settings"}, primary: []string{}},
}

// auditDTOs are data carriers: they hold no logic and should not take a slot while
// 1900 candidates compete for it.
var auditDTOs = []string{"vo.java", "dto.java", "po.java", "entity.java", "bean.java", "request.java", "response.java", "model.java", "param.java"}

// auditablePath reports whether a path is eligible for the deterministic shortlist.
func auditablePath(entry InventoryEntry) bool {
	if entry.LowValue {
		return false
	}
	lower := strings.ToLower(entry.Path)
	ext := entry.Ext
	if ext == "" {
		ext = strings.ToLower(path.Ext(lower))
	}
	if !auditSourceExts[ext] {
		return false
	}
	padded := "/" + lower
	for _, weak := range auditWeakDirSegments {
		if strings.Contains(padded, weak) {
			return false
		}
	}
	name := strings.ToLower(path.Base(lower))
	for _, suffix := range auditDTOs {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	return true
}

// auditBucketOf returns the first bucket a path belongs to (bucket order is the
// priority order) and how many of its keywords appear in the file name.
func auditBucketOf(entryPath string) (auditBucket, int, bool) {
	rawBase := path.Base(entryPath)
	lower := strings.ToLower(entryPath)
	name := strings.ToLower(rawBase)
	dir := strings.TrimSuffix(lower, name)
	_ = dir
	for _, bucket := range auditBuckets {
		hits := 0
		for _, word := range bucket.words {
			if containsSignal(rawBase, name, word) {
				if containsWord(bucket.primary, word) {
					hits += 4
				} else {
					hits++
				}
				continue
			}
			_ = dir
		}
		if hits > 0 {
			return bucket, hits, true
		}
	}
	return auditBucket{}, 0, false
}

// auditPriority is kept for diagnostics: bucket rank dominates, keyword hits refine.
func auditPriority(entry InventoryEntry) int {
	if !auditablePath(entry) {
		return 0
	}
	bucket, hits, ok := auditBucketOf(entry.Path)
	if !ok {
		return 0
	}
	rank := 0
	for i, b := range auditBuckets {
		if b.name == bucket.name {
			rank = len(auditBuckets) - i
			break
		}
	}
	return rank*10 + hits
}

// containsSignal matches a keyword on a word boundary for short tokens: plain
// substring matching turned "posSetting" into an "oss" hit and put unrelated DAOs in
// the upload bucket.
func containsSignal(rawBase, lowerBase, word string) bool {
	if len(word) > 4 {
		return strings.Contains(lowerBase, word)
	}
	for start := 0; ; {
		index := strings.Index(lowerBase[start:], word)
		if index < 0 {
			return false
		}
		index += start
		// A short token must start a word: at the beginning, after a non-letter, or
		// at a CamelCase boundary (UserDao) so "posSetting" no longer matches "oss".
		if index == 0 || !isLetter(lowerBase[index-1]) || isUpper(rawBase[index]) {
			return true
		}
		start = index + 1
		if start >= len(lowerBase) {
			return false
		}
	}
}

func isUpper(b byte) bool { return b >= 'A' && b <= 'Z' }

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// containsWord reports whether the slice contains the word.
func containsWord(words []string, word string) bool {
	for _, candidate := range words {
		if candidate == word {
			return true
		}
	}
	return false
}

// checkBucket keeps auditBuckets referenced for tests that inspect names.
func checkBucket(name string) bool {
	for _, bucket := range auditBuckets {
		if bucket.name == name {
			return true
		}
	}
	return false
}

// SuggestAuditFiles returns the highest-signal files the audit scope should hold.
// Buckets are interleaved (one crypto, one upload, one filter, …) so the plan always
// covers the control plane instead of filling up with one class of file.
func (r *Registry) SuggestAuditFiles(limit int, perDir int) []string {
	if limit <= 0 {
		return nil
	}
	if perDir <= 0 {
		perDir = 4
	}
	type candidate struct {
		path     string
		hits     int
		depth    int
		nameSize int
	}
	grouped := make([][]candidate, len(auditBuckets))
	for _, entry := range r.inventory {
		if !auditablePath(entry) {
			continue
		}
		bucket, hits, ok := auditBucketOf(entry.Path)
		if !ok {
			continue
		}
		index := 0
		for i, b := range auditBuckets {
			if b.name == bucket.name {
				index = i
				break
			}
		}
		grouped[index] = append(grouped[index], candidate{path: entry.Path, hits: hits, depth: strings.Count(entry.Path, "/"), nameSize: len(path.Base(entry.Path))})
	}
	for _, group := range grouped {
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].hits != group[j].hits {
				return group[i].hits > group[j].hits
			}
			// A short core class name (OSSController) usually holds the logic; a long
			// one (ComCampusRecruitSignUpSessionDao) is often generated plumbing.
			if group[i].nameSize != group[j].nameSize {
				return group[i].nameSize < group[j].nameSize
			}
			if group[i].depth != group[j].depth {
				return group[i].depth < group[j].depth
			}
			return group[i].path < group[j].path
		})
	}
	used := map[string]int{}
	out := make([]string, 0, limit)
	// Guarantee the best few files of every class before filling by rotation: with
	// pure rotation the per-directory cap let "…SessionDao" files take every dao/act
	// slot and pushed OSSController out of the plan entirely.
	guarantee := guaranteedPerBucket
	for _, group := range grouped {
		taken := 0
		for _, candidate := range group {
			if taken >= guarantee || len(out) >= limit {
				break
			}
			out = append(out, candidate.path)
			used[path.Dir(candidate.path)]++
			taken++
		}
	}
	seen := map[string]bool{}
	for _, p := range out {
		seen[p] = true
	}
	for round := 0; len(out) < limit; round++ {
		progressed := false
		for _, group := range grouped {
			if round >= len(group) {
				continue
			}
			progressed = true
			candidate := group[round]
			if seen[candidate.path] {
				continue
			}
			dir := path.Dir(candidate.path)
			if used[dir] >= perDir+guarantee {
				continue
			}
			used[dir]++
			seen[candidate.path] = true
			out = append(out, candidate.path)
			if len(out) >= limit {
				break
			}
		}
		if !progressed {
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
