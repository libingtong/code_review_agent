package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Learned concurrency is persisted separately from quota windows: it is a
// property of the vendor/plan (how many parallel requests it tolerates), not of
// the current 5h window.
type concurrencyState struct {
	Provider  string    `json:"provider"`
	Learned   int       `json:"learned"`
	UpdatedAt time.Time `json:"updated_at"`
}

type concurrencyFile struct {
	Version   int                `json:"version"`
	SavedAt   time.Time          `json:"saved_at"`
	Channels []concurrencyState `json:"channels"`
}

// SetConcurrencyStatePath configures where learned concurrency lives.
func (p *Pool) SetConcurrencyStatePath(path string) {
	p.mu.Lock()
	p.concurrencyPath = path
	p.mu.Unlock()
}

// noteSettle records a newly learned value and persists it immediately: these
// events are rare (once per settle window), so no debounce is needed.
func (p *Pool) noteSettle(providerID string, limit int) {
	p.appendNote(fmt.Sprintf("%s 并发稳定在 %d，已记住（下次启动即用）", providerID, limit))
	if err := p.SaveConcurrencyState(); err != nil {
		p.appendNote("保存并发学习值失败：" + err.Error())
	}
}

// SaveConcurrencyState writes every channel's learned value.
func (p *Pool) SaveConcurrencyState() error {
	p.mu.RLock()
	path := p.concurrencyPath
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	file := concurrencyFile{Version: 1, SavedAt: p.now()}
	for _, provider := range providers {
		if provider.limiter == nil {
			continue
		}
		if learned := provider.limiter.Learned(); learned > 0 {
			file.Channels = append(file.Channels, concurrencyState{Provider: provider.ID(), Learned: learned, UpdatedAt: p.now()})
		}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".concurrency-*")
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

// LoadConcurrencyState applies persisted values; a channel without an entry
// keeps starting from initial_in_flight.
func (p *Pool) LoadConcurrencyState() error {
	p.mu.RLock()
	path := p.concurrencyPath
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var file concurrencyFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析并发学习状态失败：%w", err)
	}
	if file.Version != 1 {
		return fmt.Errorf("不支持的并发学习状态版本 %d", file.Version)
	}
	for _, entry := range file.Channels {
		for _, provider := range providers {
			if provider.ID() == entry.Provider && provider.limiter != nil {
				provider.limiter.SetLearned(entry.Learned)
			}
		}
	}
	return nil
}

// ResetLearned forgets one channel's value (or all when id is empty) and
// persists the change.
func (p *Pool) ResetLearned(id string) error {
	p.mu.RLock()
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	found := false
	for _, provider := range providers {
		if id != "" && provider.ID() != id {
			continue
		}
		if provider.limiter != nil {
			provider.limiter.ForgetLearned()
			found = true
		}
	}
	if !found {
		return fmt.Errorf("未知的渠道 %q", id)
	}
	return p.SaveConcurrencyState()
}

// LearnedConcurrency reports the remembered value for one channel (0 = none).
func (p *Pool) LearnedConcurrency(id string) int {
	provider := p.find(id)
	if provider == nil || provider.limiter == nil {
		return 0
	}
	return provider.limiter.Learned()
}
