package pool

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
)

// BuildRegistry constructs a model registry from config upstream sections.
func BuildRegistry(cfg *config.Config) *registry.Registry {
	r := registry.New()
	now := time.Now().Unix()

	for i, p := range cfg.AnthropicMessages {
		name := providerName(p.Name, "anthropic", i)
		keys := expandProvider("claude", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.Speed, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "claude",
			}, keysForModel(keys, m.Name))
		}
	}

	for i, p := range cfg.OpenAIResponses {
		name := providerName(p.Name, "responses", i)
		keys := expandProvider("openai-response", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.Speed, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "openai-response",
			}, keysForModel(keys, m.Name))
		}
	}

	for i, p := range cfg.OpenAICompletions {
		name := providerName(p.Name, "compat", i)
		keys := expandProvider("openai", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.Speed, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "openai",
			}, keysForModel(keys, m.Name))
		}
	}
	return r
}

func providerName(name, fallbackPrefix string, index int) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	return fmt.Sprintf("%s-%d", fallbackPrefix, index)
}

func expandProvider(provider, name, baseURL, flatKey, flatProxy string, flatPriority int, headers map[string]string, speed string, entries []config.APIKeyEntry, globalProxy, failoverMode string) []registry.UpstreamKey {
	baseURL = trimSlash(baseURL)
	failoverMode = config.NormalizeFailoverMode(failoverMode)
	expanded := config.ExpandKeys(flatKey, entries)
	out := make([]registry.UpstreamKey, 0, len(expanded))
	// proxy is provider-level (flatProxy), then global fallback
	proxy := flatProxy
	if proxy == "" {
		proxy = globalProxy
	}
	for i, e := range expanded {
		h := map[string]string{}
		for k, v := range headers {
			h[k] = v
		}
		out = append(out, registry.UpstreamKey{
			ID:            fmt.Sprintf("%s-%d", name, i),
			Name:          name,
			Provider:      provider,
			BaseURL:       baseURL,
			APIKey:        e.APIKey,
			Priority:      flatPriority,
			EntryPriority: e.Priority,
			Speed:         speed,
			Headers:       h,
			ProxyURL:      proxy,
			FailoverMode:  failoverMode,
		})
	}
	return out
}

func keysForModel(keys []registry.UpstreamKey, upstreamModel string) []registry.UpstreamKey {
	// Attach upstream model name into a copy via ID suffix is not needed;
	// handlers resolve alias separately. Store upstream model in a synthetic header.
	out := make([]registry.UpstreamKey, len(keys))
	for i, k := range keys {
		out[i] = k
		if out[i].Headers == nil {
			out[i].Headers = map[string]string{}
		} else {
			h := make(map[string]string, len(k.Headers)+1)
			for kk, vv := range k.Headers {
				h[kk] = vv
			}
			out[i].Headers = h
		}
		if upstreamModel != "" {
			out[i].Headers["x-lite-upstream-model"] = upstreamModel
		}
	}
	return out
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Selector does round-robin across keys for a model with failure skip.
type Selector struct {
	reg   *registry.Registry
	rr    sync.Map // model -> *uint64
	retry int
}

func NewSelector(reg *registry.Registry, retry int) *Selector {
	return &Selector{reg: reg, retry: retry}
}

// Pick chooses the next unused key for model.
// preferSupplier (if non-empty) prefers remaining keys from that provider Name first.
// skipSuppliers excludes all keys under those provider Names (dead relay).
// Selection order: choose the lowest provider Priority first (round-robin among
// provider ties), then choose the lowest EntryPriority only within that provider.
func (s *Selector) Pick(model string, tried map[string]struct{}, preferSupplier string, skipSuppliers map[string]struct{}) (registry.UpstreamKey, string, error) {
	_, keys, ok := s.reg.Resolve(model)
	if !ok || len(keys) == 0 {
		return registry.UpstreamKey{}, "", fmt.Errorf("model not found: %s", model)
	}
	start := s.nextIndex(model, len(keys))
	ordered := make([]registry.UpstreamKey, 0, len(keys))
	for i := range len(keys) {
		ordered = append(ordered, keys[(start+i)%len(keys)])
	}

	tryPick := func(restrictSupplier string) (registry.UpstreamKey, bool) {
		selectedSupplier := ""
		bestProviderPri := int(^uint(0) >> 1)
		for _, k := range ordered {
			if tried != nil {
				if _, used := tried[k.ID]; used {
					continue
				}
			}
			if restrictSupplier != "" && k.Name != restrictSupplier {
				continue
			}
			if skipSuppliers != nil {
				if _, skip := skipSuppliers[k.Name]; skip {
					continue
				}
			}
			if k.Priority < bestProviderPri {
				selectedSupplier = k.Name
				bestProviderPri = k.Priority
			}
		}
		if selectedSupplier == "" {
			return registry.UpstreamKey{}, false
		}

		var best registry.UpstreamKey
		bestEntryPri := int(^uint(0) >> 1)
		for _, k := range ordered {
			if k.Name != selectedSupplier {
				continue
			}
			if tried != nil {
				if _, used := tried[k.ID]; used {
					continue
				}
			}
			if k.EntryPriority < bestEntryPri {
				best = k
				bestEntryPri = k.EntryPriority
			}
		}
		return best, true
	}

	if preferSupplier != "" {
		if k, ok := tryPick(preferSupplier); ok {
			return withUpstreamModel(k, model)
		}
	}
	if k, ok := tryPick(""); ok {
		return withUpstreamModel(k, model)
	}
	return registry.UpstreamKey{}, "", fmt.Errorf("no available credentials for model %s", model)
}

func withUpstreamModel(k registry.UpstreamKey, fallback string) (registry.UpstreamKey, string, error) {
	upstreamModel := fallback
	if k.Headers != nil {
		if m := k.Headers["x-lite-upstream-model"]; m != "" {
			upstreamModel = m
		}
	}
	return k, upstreamModel, nil
}

func (s *Selector) nextIndex(model string, n int) int {
	if n <= 0 {
		return 0
	}
	v, _ := s.rr.LoadOrStore(model, new(uint64))
	counter := v.(*uint64)
	return int(atomic.AddUint64(counter, 1)-1) % n
}

func (s *Selector) MaxAttempts(model string) int {
	_, keys, ok := s.reg.Resolve(model)
	if !ok {
		return 1
	}
	max := 1 + s.retry
	if max > len(keys) {
		max = len(keys)
	}
	if max < 1 {
		max = 1
	}
	return max
}
