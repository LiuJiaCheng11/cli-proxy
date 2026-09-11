package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The catalog is account-specific: entries carry an enable flag describing what
// the current plan can use, so it is cached per credential rather than globally.
const catalogTTL = 5 * time.Minute

// catalogModel is one entry of the upstream model list.
type catalogModel struct {
	Key            string `json:"key"`
	DisplayName    string `json:"display_name"`
	Enable         bool   `json:"enable"`
	IsFree         bool   `json:"is_free"`
	IsDefault      bool   `json:"is_default"`
	IsReasoning    bool   `json:"is_reasoning"`
	IsVL           bool   `json:"is_vl"`
	MaxInputTokens int64  `json:"max_input_tokens"`
}

// catalog is the grouped model list. Only "chat" is published, but every group
// feeds the display-name → key lookup used when a client names a model.
type catalog struct {
	Chat   []catalogModel
	byName map[string]string
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]cachedCatalog{}
)

type cachedCatalog struct {
	catalog   *catalog
	fetchedAt time.Time
}

// getCatalog returns the credential's model list, refreshing it at most every
// catalogTTL. A failed refresh keeps the previous snapshot.
func getCatalog(sa *storedAuth) (*catalog, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	key := sa.UID
	if c, ok := catalogCache[key]; ok && time.Since(c.fetchedAt) < catalogTTL {
		return c.catalog, nil
	}
	cat, err := fetchCatalog(sa)
	if err != nil {
		if c, ok := catalogCache[key]; ok {
			return c.catalog, nil
		}
		return nil, err
	}
	catalogCache[key] = cachedCatalog{catalog: cat, fetchedAt: time.Now()}
	return cat, nil
}

func fetchCatalog(sa *storedAuth) (*catalog, error) {
	sess, err := sa.session()
	if err != nil {
		return nil, err
	}
	raw, err := sess.callJSON("GET", endpointsFor(sa.Variant).modelAPI+pathModels, nil, "application/json")
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	cat := &catalog{byName: map[string]string{}}
	for group, list := range raw {
		entries, ok := list.([]any)
		if !ok {
			continue
		}
		for _, item := range entries {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			m := catalogModel{
				Key:            stringField(obj, "key"),
				DisplayName:    stringField(obj, "display_name"),
				Enable:         boolField(obj, "enable"),
				IsFree:         boolField(obj, "is_free"),
				IsDefault:      boolField(obj, "is_default"),
				IsReasoning:    boolField(obj, "is_reasoning"),
				IsVL:           boolField(obj, "is_vl"),
				MaxInputTokens: intField(obj, "max_input_tokens"),
			}
			if m.DisplayName != "" && m.Key != "" {
				cat.byName[m.DisplayName] = m.Key
			}
			if group == "chat" && m.Key != "" {
				cat.Chat = append(cat.Chat, m)
			}
		}
	}
	if len(cat.Chat) == 0 {
		return nil, fmt.Errorf("models: upstream returned no chat models")
	}
	return cat, nil
}

// resolveModelKey maps a client-supplied model name onto the upstream key. The
// catalog is keyed by display name, so a raw key passes straight through.
func (c *catalog) resolveModelKey(model string) string {
	if c == nil || model == "" {
		return model
	}
	if key, ok := c.byName[model]; ok {
		return key
	}
	return model
}

// qoderModels publishes the chat group. The upstream decides what is in it and
// whether it is enabled for this account, so nothing is filtered here.
func qoderModels(sa *storedAuth) []pluginapi.ModelInfo {
	cat, err := getCatalog(sa)
	if err != nil || cat == nil {
		return nil
	}
	models := make([]pluginapi.ModelInfo, 0, len(cat.Chat))
	for _, m := range cat.Chat {
		id := m.DisplayName
		if id == "" {
			id = m.Key
		}
		modalities := []string{"text"}
		if m.IsVL {
			modalities = append(modalities, "image")
		}
		models = append(models, pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                firstNonEmpty(m.DisplayName, m.Key),
			Name:                       m.Key,
			SupportedGenerationMethods: []string{"chat"},
			SupportedInputModalities:   modalities,
			ContextLength:              m.MaxInputTokens,
			InputTokenLimit:            m.MaxInputTokens,
			UserDefined:                true,
		})
	}
	return models
}

func stringField(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

func boolField(obj map[string]any, key string) bool {
	v, ok := obj[key].(bool)
	return ok && v
}

func intField(obj map[string]any, key string) int64 {
	switch v := obj[key].(type) {
	case float64:
		return int64(v)
	case bool:
		if v {
			return 1
		}
	}
	return 0
}
