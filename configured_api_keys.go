package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	configuredAPIKeysCacheTTL = 3 * time.Second
	maxConfiguredAPIKeysBody  = 1 << 20
)

type hostAPIKeyHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type configuredAPIKeyIdentity struct {
	ID          string
	CallerScope string
	APIKeyRef   string
	MaskedKey   string
}

type configuredAPIKeyCache struct {
	fetchedAt  time.Time
	configHash [32]byte
	items      []configuredAPIKeyIdentity
}

type hostAPIKeysResponse struct {
	APIKeys []string `json:"api-keys"`
}

func (r *pluginRuntime) invalidateConfiguredAPIKeys() {
	r.configuredKeysMu.Lock()
	r.configuredKeys = configuredAPIKeyCache{}
	r.configuredKeysMu.Unlock()
}

func (r *pluginRuntime) configuredAPIKeyIdentities() ([]configuredAPIKeyIdentity, error) {
	r.mu.RLock()
	config := r.config
	crypto := r.crypto
	generation := r.apiKeyGeneration
	client := r.hostAPIKeyClient
	r.mu.RUnlock()
	if config.ManagementAPIURL == "" || config.ManagementAPIKey == "" {
		return nil, nil
	}
	if !crypto.enabled || generation == 0 {
		return nil, fmt.Errorf("API key tracking must be enabled before syncing configured keys")
	}
	configHash := sha256.Sum256([]byte(config.ManagementAPIURL + "\x00" + config.ManagementAPIKey))
	now := time.Now().UTC()
	r.configuredKeysMu.Lock()
	defer r.configuredKeysMu.Unlock()
	if r.configuredKeys.configHash == configHash && !r.configuredKeys.fetchedAt.IsZero() && now.Sub(r.configuredKeys.fetchedAt) < configuredAPIKeysCacheTTL {
		return append([]configuredAPIKeyIdentity(nil), r.configuredKeys.items...), nil
	}
	if client == nil {
		client = &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	request, err := http.NewRequest(http.MethodGet, config.ManagementAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create configured API key request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+config.ManagementAPIKey)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch configured API keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch configured API keys: HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxConfiguredAPIKeysBody+1))
	var payload hostAPIKeysResponse
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode configured API keys: %w", err)
	}
	if len(payload.APIKeys) > maxAPIKeyQuotas {
		return nil, fmt.Errorf("configured API key count exceeds %d", maxAPIKeyQuotas)
	}
	items := make([]configuredAPIKeyIdentity, 0, len(payload.APIKeys))
	seen := make(map[string]struct{}, len(payload.APIKeys))
	for _, apiKey := range payload.APIKeys {
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		scope := downstreamCallerScope(apiKey)
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		fingerprint := apiKeyFingerprint(apiKey, crypto.indexKey)
		items = append(items, configuredAPIKeyIdentity{
			ID:          quotaID(scope),
			CallerScope: scope,
			APIKeyRef:   apiKeyRef(generation, fingerprint),
			MaskedKey:   maskedAPIKey(apiKey),
		})
	}
	r.configuredKeys = configuredAPIKeyCache{fetchedAt: now, configHash: configHash, items: append([]configuredAPIKeyIdentity(nil), items...)}
	return items, nil
}
