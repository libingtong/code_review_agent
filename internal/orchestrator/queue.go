// Package orchestrator runs a queue of audit tasks one after another, pausing
// between them for the working-hours window and provider quota.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const queueVersion = 1

type TaskStatus string

const (
	StatusPending TaskStatus = "pending"
	StatusRunning TaskStatus = "running"
	StatusPaused  TaskStatus = "paused"
	StatusDone    TaskStatus = "done"
	StatusFailed  TaskStatus = "failed"
	StatusSkipped TaskStatus = "skipped"
)

type TaskBudget struct {
	Infinite *bool `yaml:"infinite" json:"infinite,omitempty"`
	Hours    int   `yaml:"hours" json:"hours,omitempty"`
	Minutes  int   `yaml:"minutes" json:"minutes,omitempty"`
	Tokens   int64 `yaml:"tokens" json:"tokens,omitempty"`
}

type TaskAgents struct {
	Recon int `yaml:"recon" json:"recon,omitempty"`
	Audit int `yaml:"audit" json:"audit,omitempty"`
}

type Task struct {
	ID     string     `yaml:"id" json:"id"`
	Title  string     `yaml:"title" json:"title"`
	Dir    string     `yaml:"dir" json:"dir"`
	Prompt string     `yaml:"prompt" json:"prompt,omitempty"`
	Budget TaskBudget `yaml:"budget" json:"budget,omitempty"`
	Agents TaskAgents `yaml:"agents" json:"agents,omitempty"`
	// Retries: 0 inherits orchestrator.retries, a negative value disables
	// retrying for this task.
	Retries   int        `yaml:"retries" json:"retries,omitempty"`
	Status    TaskStatus `yaml:"-" json:"status"`
	Attempts  int        `yaml:"-" json:"attempts,omitempty"`
	Session   string     `yaml:"-" json:"session,omitempty"`
	Report    string     `yaml:"-" json:"report,omitempty"`
	StartedAt string     `yaml:"-" json:"started_at,omitempty"`
	EndedAt   string     `yaml:"-" json:"ended_at,omitempty"`
	Error     string     `yaml:"-" json:"error,omitempty"`
}

// Queue is the persisted orchestration state. Cursor points at the task that was
// running (or is next), which is what makes a restart resumable.
type Queue struct {
	Version int    `json:"version"`
	SavedAt string `json:"saved_at"`
	Cursor  int    `json:"cursor"`
	Tasks   []Task `json:"tasks"`
}

func newQueue() *Queue { return &Queue{Version: queueVersion, Tasks: []Task{}} }

func LoadQueue(path string) (*Queue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newQueue(), nil
		}
		return nil, err
	}
	var queue Queue
	if err := json.Unmarshal(data, &queue); err != nil {
		return nil, fmt.Errorf("解析任务队列失败：%w", err)
	}
	if queue.Version != queueVersion {
		return nil, fmt.Errorf("不支持的任务队列版本 %d", queue.Version)
	}
	queue.normalize()
	return &queue, nil
}

func (q *Queue) normalize() {
	if q.Tasks == nil {
		q.Tasks = []Task{}
	}
	used := map[string]bool{}
	for i := range q.Tasks {
		task := &q.Tasks[i]
		if strings.TrimSpace(task.ID) == "" {
			task.ID = fmt.Sprintf("T%03d", i+1)
		}
		for used[task.ID] {
			task.ID += "-1"
		}
		used[task.ID] = true
		if task.Title == "" {
			task.Title = filepath.Base(task.Dir)
		}
		switch task.Status {
		case StatusRunning, StatusPaused:
			// A task interrupted by a crash is resumable, not finished.
			task.Status = StatusPending
		case "":
			task.Status = StatusPending
		}
		if task.Dir != "" {
			if abs, err := filepath.Abs(task.Dir); err == nil {
				task.Dir = abs
			}
		}
	}
	if q.Cursor < 0 || q.Cursor > len(q.Tasks) {
		q.Cursor = 0
	}
}

// Save writes the queue atomically so a crash cannot truncate the plan.
func (q *Queue) Save(path string) error {
	q.Version = queueVersion
	q.SavedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tasks-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (q *Queue) byID(id string) *Task {
	for i := range q.Tasks {
		if q.Tasks[i].ID == id {
			return &q.Tasks[i]
		}
	}
	return nil
}

// next returns the first task that still needs work, preferring the cursor so a
// resumed run continues where it stopped.
func (q *Queue) next() *Task {
	for i := q.Cursor; i < len(q.Tasks); i++ {
		if q.Tasks[i].Status == StatusPending || q.Tasks[i].Status == StatusPaused {
			q.Cursor = i
			return &q.Tasks[i]
		}
	}
	for i := 0; i < q.Cursor && i < len(q.Tasks); i++ {
		if q.Tasks[i].Status == StatusPending || q.Tasks[i].Status == StatusPaused {
			q.Cursor = i
			return &q.Tasks[i]
		}
	}
	return nil
}

func (q *Queue) counts() map[TaskStatus]int {
	out := map[TaskStatus]int{}
	for _, task := range q.Tasks {
		out[task.Status]++
	}
	return out
}

// remaining reports how many tasks still need work, for the status line.
func (q *Queue) remaining() int {
	total := 0
	for _, task := range q.Tasks {
		if task.Status == StatusPending || task.Status == StatusPaused || task.Status == StatusRunning {
			total++
		}
	}
	return total
}

// importPath accepts a YAML/JSON queue, a plain text list, or a directory.
// A directory is one task by default (a single repository); eachSubdir turns
// it into a root of repositories.
func importPath(path string, eachSubdir bool) ([]Task, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			return nil, absErr
		}
		if !eachSubdir {
			return []Task{{Dir: abs}}, nil
		}
		return importDir(abs)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return importJSON(data)
	case ".yaml", ".yml":
		return importYAML(data)
	}
	return importText(string(data))
}

func importJSON(data []byte) ([]Task, error) {
	var wrapper struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Tasks) > 0 {
		return wrapper.Tasks, nil
	}
	var list []Task
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("任务文件既不是 {tasks:[...]} 也不是数组：%w", err)
	}
	return list, nil
}

func importYAML(data []byte) ([]Task, error) {
	var wrapper struct {
		Tasks []Task `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err == nil && len(wrapper.Tasks) > 0 {
		return wrapper.Tasks, nil
	}
	var list []Task
	if err := yaml.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("任务文件既不是 tasks: 列表也不是数组：%w", err)
	}
	return list, nil
}

// importText accepts one task per line: "dir" or "dir | optional prompt".
func importText(text string) ([]Task, error) {
	var tasks []Task
	for number, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		dir, prompt, _ := strings.Cut(line, "|")
		dir = strings.TrimSpace(strings.Trim(dir, `"'`))
		if dir == "" {
			return nil, fmt.Errorf("第 %d 行缺少目录", number+1)
		}
		tasks = append(tasks, Task{Dir: dir, Prompt: strings.TrimSpace(prompt)})
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("任务文件没有任何有效行")
	}
	return tasks, nil
}

func importDir(root string) ([]Task, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirs = append(dirs, filepath.Join(root, entry.Name()))
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("%s 下没有子目录", root)
	}
	sort.Strings(dirs)
	tasks := make([]Task, 0, len(dirs))
	for _, dir := range dirs {
		tasks = append(tasks, Task{Dir: dir})
	}
	return tasks, nil
}
