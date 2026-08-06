package registry

import (
	"sync"
	"time"
)

// ModelInfo is the minimal model metadata used by translators and /v1/models.
type ModelInfo struct {
	ID          string           `json:"id"`
	Object      string           `json:"object"`
	Created     int64            `json:"created"`
	OwnedBy     string           `json:"owned_by"`
	Type        string           `json:"type,omitempty"`
	Thinking    *ThinkingSupport `json:"thinking,omitempty"`
	UserDefined bool             `json:"-"`
}

// ThinkingSupport describes optional reasoning levels/budgets.
type ThinkingSupport struct {
	Min            int      `json:"min,omitempty"`
	Max            int      `json:"max,omitempty"`
	ZeroAllowed    bool     `json:"zero_allowed,omitempty"`
	DynamicAllowed bool     `json:"dynamic_allowed,omitempty"`
	Levels         []string `json:"levels,omitempty"`
}

// UpstreamKey is one credential that can serve a model.
type UpstreamKey struct {
	ID            string
	Name          string // config provider name (profile id)
	Provider      string // openai | openai-response | claude
	BaseURL       string
	APIKey        string
	Priority      int    // provider-level priority (lower = higher); used across the merged model pool
	EntryPriority int    // key-level priority within the same provider Name (lower = higher)
	Speed         string // model-level fast tier; empty blocks client-selected tiers
	Verbosity     string // model-level Responses hint (low|medium|high) injected as text.verbosity
	Headers       map[string]string
	ProxyURL      string
	FailoverMode  string // key | provider (from provider config)
}

type modelEntry struct {
	info *ModelInfo
	keys []UpstreamKey
}

// Registry maps model aliases to upstream keys.
type Registry struct {
	mu     sync.RWMutex
	models map[string]*modelEntry // alias/name -> entry
}

func New() *Registry {
	return &Registry{models: make(map[string]*modelEntry)}
}

func (r *Registry) RegisterModel(alias string, info *ModelInfo, keys []UpstreamKey) {
	if alias == "" || info == nil || len(keys) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]UpstreamKey, len(keys))
	copy(cp, keys)

	if existing, ok := r.models[alias]; ok && existing != nil {
		// Same client alias from multiple suppliers: merge credential pools.
		merged := make([]UpstreamKey, 0, len(existing.keys)+len(cp))
		merged = append(merged, existing.keys...)
		merged = append(merged, cp...)
		existing.keys = merged
		return
	}

	infoCopy := *info
	if infoCopy.Object == "" {
		infoCopy.Object = "model"
	}
	if infoCopy.Created == 0 {
		infoCopy.Created = time.Now().Unix()
	}
	if infoCopy.OwnedBy == "" {
		infoCopy.OwnedBy = "lite-cpa"
	}
	infoCopy.UserDefined = true
	r.models[alias] = &modelEntry{info: &infoCopy, keys: cp}
}

func (r *Registry) Resolve(model string) (*ModelInfo, []UpstreamKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.models[model]
	if !ok {
		return nil, nil, false
	}
	keys := make([]UpstreamKey, len(e.keys))
	copy(keys, e.keys)
	info := *e.info
	return &info, keys, true
}

func (r *Registry) List() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelInfo, 0, len(r.models))
	for _, e := range r.models {
		out = append(out, *e.info)
	}
	return out
}

// ReplaceFrom atomically replaces this registry's model map with a clone of
// other. Used by config hot-reload so readers always see a consistent set.
func (r *Registry) ReplaceFrom(other *Registry) {
	if other == nil {
		r.mu.Lock()
		r.models = make(map[string]*modelEntry)
		r.mu.Unlock()
		return
	}
	other.mu.RLock()
	next := make(map[string]*modelEntry, len(other.models))
	for k, v := range other.models {
		next[k] = v
	}
	other.mu.RUnlock()
	r.mu.Lock()
	r.models = next
	r.mu.Unlock()
}

// LookupModelInfo is used by ported translators for thinking capability checks.
// Lite treats all config models as user-defined adaptive-capable.
func LookupModelInfo(modelID string, provider ...string) *ModelInfo {
	_ = provider
	return &ModelInfo{
		ID:          modelID,
		UserDefined: true,
		Thinking: &ThinkingSupport{
			Min:            0,
			Max:            128000,
			ZeroAllowed:    true,
			DynamicAllowed: true,
			Levels:         []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "auto"},
		},
	}
}
