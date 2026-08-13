package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var version = "dev"

// maxSupportedRPCSchema is intentionally independent from the SDK's latest
// schema. Future hosts may negotiate down to this verified contract without
// the plugin claiming support for semantics it has not implemented.
const maxSupportedRPCSchema uint32 = 3

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type pluginRuntime struct {
	lifecycleMu      sync.Mutex
	priceSyncMu      sync.Mutex
	mu               sync.RWMutex
	store            *Store
	config           Config
	routes           registeredRoutes
	modelsDevFetcher *modelsDevFetcher
	exchangeRates    *exchangeRateService
	authResolver     *authIdentityResolver
	priceSyncing     bool
	fullModeMu       sync.Mutex
	fullModeSessions map[[32]byte]fullModeSession
	fullModeUploads  map[string]fullModeUpload
}

var runtimeState = &pluginRuntime{}

func (r *pluginRuntime) register(raw []byte) (registration, error) {
	request, config, err := decodeLifecycle(raw)
	if err != nil {
		return registration{}, err
	}

	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if err := r.applyConfig(config); err != nil {
		return registration{}, err
	}
	return pluginRegistration(negotiateRPCSchema(request.SchemaVersion)), nil
}

func (r *pluginRuntime) reconfigure(raw []byte) (registration, error) {
	request, config, err := decodeLifecycle(raw)
	if err != nil {
		return registration{}, err
	}

	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if err := r.applyConfig(config); err != nil {
		return registration{}, err
	}
	return pluginRegistration(negotiateRPCSchema(request.SchemaVersion)), nil
}

func negotiateRPCSchema(hostSchema uint32) uint32 {
	if hostSchema == 0 {
		return 1
	}
	if hostSchema < maxSupportedRPCSchema {
		return hostSchema
	}
	return maxSupportedRPCSchema
}

func (r *pluginRuntime) applyConfig(config Config) error {
	r.mu.RLock()
	current := r.store
	currentConfig := r.config
	r.mu.RUnlock()

	if current != nil && currentConfig.DataPath == config.DataPath {
		if err := current.Reconfigure(config); err != nil {
			return err
		}
		r.mu.Lock()
		r.config = config
		r.mu.Unlock()
		return nil
	}

	next, err := openStore(config)
	if err != nil {
		return err
	}
	r.mu.Lock()
	old := r.store
	r.store = next
	r.config = config
	r.mu.Unlock()
	r.fullModeMu.Lock()
	r.fullModeSessions = nil
	r.fullModeUploads = nil
	r.fullModeMu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			return fmt.Errorf("close previous store: %w", err)
		}
	}
	return nil
}

func (r *pluginRuntime) handleUsage(raw []byte) (map[string]any, error) {
	usage, err := decodeUsage(raw, nowUTC())
	if err != nil {
		return nil, withStatus(400, "%v", err)
	}
	r.resolveUsageIdentity(&usage)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.store == nil {
		return nil, withStatus(503, "plugin storage is not initialized")
	}
	if err := r.store.Record(usage); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

func (r *pluginRuntime) shutdown() error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.Lock()
	store := r.store
	r.store = nil
	r.config = Config{}
	r.routes = registeredRoutes{}
	r.exchangeRates = nil
	r.authResolver = nil
	r.mu.Unlock()
	r.fullModeMu.Lock()
	r.fullModeSessions = nil
	r.fullModeUploads = nil
	r.fullModeMu.Unlock()
	if store == nil {
		return nil
	}
	return store.Close()
}

func decodeLifecycle(raw []byte) (lifecycleRequest, Config, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var envelope struct {
		ConfigYAML    json.RawMessage `json:"config_yaml"`
		SchemaVersion uint32          `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return lifecycleRequest{}, Config{}, fmt.Errorf("decode lifecycle request: %w", err)
	}
	configYAML, err := decodeLifecycleConfigYAML(envelope.ConfigYAML)
	if err != nil {
		return lifecycleRequest{}, Config{}, err
	}
	request := lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: envelope.SchemaVersion}
	config, err := parseConfig(request.ConfigYAML)
	if err != nil {
		return lifecycleRequest{}, Config{}, err
	}
	return request, config, nil
}

// decodeLifecycleConfigYAML accepts the host's standard base64 encoding of a
// []byte field, while remaining compatible with hosts/tools that send a plain
// YAML string or an explicit JSON byte array.
func decodeLifecycleConfigYAML(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(text); decodeErr == nil && strings.Contains(string(decoded), ":") {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return bytes, nil
	}
	return nil, fmt.Errorf("config_yaml must be a base64/plain string or byte array")
}

func pluginRegistration(schemaVersion uint32) registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "CAP Token Usage Tracker",
			Version:          version,
			Author:           "AITNR",
			GitHubRepository: "https://github.com/AITNR/cap-token-usage-tracker",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "data_path", Type: pluginapi.ConfigFieldTypeString, Description: "bbolt database path; defaults to the data directory next to the discovered plugins directory, while explicit relative paths use the CLIProxyAPI working directory."},
				{Name: "retention_days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Number of UTC days of minute-level statistics and request details to retain (1-3650)."},
				{Name: "flush_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Maximum delay before batched statistics are flushed, for example 5s."},
				{Name: "flush_max_records", Type: pluginapi.ConfigFieldTypeInteger, Description: "Flush after this many accepted usage records."},
				{Name: "sync_on_record", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Commit every usage record before acknowledging it."},
			},
		},
		Capabilities: registrationCapabilities{UsagePlugin: true, ManagementAPI: true},
	}
}

var nowUTC = func() time.Time { return time.Now().UTC() }
