package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Selection is the user's explicit choice of channel (and optionally a model
// override), remembered across runs: "/mode -> Enter" should still be in force
// after a restart.
type Selection struct {
	Version int               `json:"version"`
	Active  string            `json:"active,omitempty"`
	Models  map[string]string `json:"models,omitempty"`
	SavedAt string            `json:"saved_at,omitempty"`
}

// SetSelectionPath configures where the choice is persisted.
func (p *Pool) SetSelectionPath(path string) {
	p.mu.Lock()
	p.selectionPath = path
	p.mu.Unlock()
}

// SaveSelection writes the current active channel and model overrides.
func (p *Pool) SaveSelection() error {
	p.mu.RLock()
	path := p.selectionPath
	active := p.active
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	selection := Selection{Version: 1, Active: active, Models: map[string]string{}, SavedAt: p.now().Format("2006-01-02T15:04:05Z07:00")}
	for _, provider := range providers {
		if model, ok := p.modelOverrides[provider.ID()]; ok && model != "" {
			selection.Models[provider.ID()] = model
		}
	}
	if len(selection.Models) == 0 {
		selection.Models = nil
	}
	data, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".selection-*")
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

// LoadSelection applies a previous choice. A channel that no longer exists or is
// disabled is ignored rather than failing startup.
func (p *Pool) LoadSelection() error {
	p.mu.RLock()
	path := p.selectionPath
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
	var selection Selection
	if err := json.Unmarshal(data, &selection); err != nil {
		return fmt.Errorf("解析模式选择失败：%w", err)
	}
	if selection.Version != 1 {
		return fmt.Errorf("不支持的模式选择版本 %d", selection.Version)
	}
	for id, model := range selection.Models {
		if err := p.SetModel(id, model); err != nil {
			p.appendNote(fmt.Sprintf("恢复 %s 的模型失败：%v", id, err))
		}
	}
	if selection.Active != "" {
		if err := p.SetActive(selection.Active); err != nil {
			p.appendNote(fmt.Sprintf("上次选择的渠道 %s 不可用，保持当前渠道", selection.Active))
		}
	}
	return nil
}

// SetModel overrides one channel's model at runtime (client rebuilt) and
// remembers it for the next start.
func (p *Pool) SetModel(id, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("模型名不能为空")
	}
	provider, err := p.byID(id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	provider.cfg.OpenAI.Model = model
	provider.client = rebuildClient(provider.cfg, p.compressCfg)
	provider.aux = auxClient(p.compressCfg, provider.cfg.OpenAI)
	if p.modelOverrides == nil {
		p.modelOverrides = map[string]string{}
	}
	p.modelOverrides[id] = model
	p.mu.Unlock()
	p.appendNote(fmt.Sprintf("%s 的模型改为 %s（已记住）", id, model))
	return p.SaveSelection()
}

// ModelOf reports the model currently used by a channel.
func (p *Pool) ModelOf(id string) string {
	provider := p.find(id)
	if provider == nil {
		return ""
	}
	return provider.cfg.OpenAI.Model
}

// ClearModelOverride forgets a runtime model override (the configured value
// returns on the next start).
func (p *Pool) ClearModelOverride(id string) error {
	provider, err := p.byID(id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.modelOverrides, id)
	p.mu.Unlock()
	if provider.cfg.OpenAI.Model == "" {
		return fmt.Errorf("渠道 %s 没有可用模型", id)
	}
	return p.SaveSelection()
}

// ClearSelection forgets the persisted choice: the next start picks by weight.
// Model overrides are dropped as well, so the configured values come back.
func (p *Pool) ClearSelection() error {
	p.mu.Lock()
	path := p.selectionPath
	for _, provider := range p.providers {
		provider.cfg.OpenAI.Model = p.configuredModels[provider.ID()]
		provider.client = rebuildClient(provider.cfg, p.compressCfg)
		provider.aux = auxClient(p.compressCfg, provider.cfg.OpenAI)
	}
	p.modelOverrides = map[string]string{}
	active := p.active
	previous := active
	p.active = ""
	p.activeSince = p.now()
	// Re-pick by weight/ratio, exactly like a fresh start.
	candidates := p.orderLocked(nil)
	if len(candidates) > 0 {
		p.active = candidates[0].ID()
	}
	active = p.active
	p.mu.Unlock()
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if previous != active {
		p.notifySwitch(previous, active, "已清除选择，按权重重新选源")
	}
	return nil
}
