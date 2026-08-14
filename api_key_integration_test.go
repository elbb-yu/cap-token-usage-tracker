package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAPIKeyTrackingRedactionRevealFilteringAndBackup(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = defaultAPIKeySecret
	config.SyncOnRecord = true
	crypto, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStoreWithCrypto(config, crypto)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{
		store:  store,
		config: config,
		crypto: crypto,
		routes: registeredRoutes{
			pluginID:                 "test",
			resourceStatsPath:        "/v0/resource/plugins/test/stats",
			resourceStatsInitialPath: "/v0/resource/plugins/test/stats/initial",
			resourceStatsTrendPath:   "/v0/resource/plugins/test/stats/trends",
			resourceStatsGroupsPath:  "/v0/resource/plugins/test/stats/groups",
			resourceRequestsPath:     "/v0/resource/plugins/test/requests",
			resourceCostsPath:        "/v0/resource/plugins/test/costs",
			fullModeDataPath:         "/v0/resource/plugins/test/full-mode/data",
		},
	}
	defer runtime.shutdown()

	keyA := "test-client-key-alpha"
	keyB := "test-client-key-beta"
	for index, key := range []string{keyA, keyA, keyB} {
		record := pluginapi.UsageRecord{
			Provider:    "test",
			Model:       "model-" + key[len(key)-4:],
			Source:      "cli",
			APIKey:      key,
			RequestedAt: time.Now().UTC().Add(time.Duration(index-3) * time.Second),
			Detail: pluginapi.UsageDetail{
				InputTokens:  int64(index + 1),
				OutputTokens: 1,
				TotalTokens:  int64(index + 2),
			},
		}
		raw, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err := runtime.handleUsage(raw); err != nil {
			t.Fatal(err)
		}
	}

	page, err := store.QueryRequests("24h", 0, 100, "")
	if err != nil || page.Total != 3 {
		t.Fatalf("request page = %+v, %v", page, err)
	}
	hashA := apiKeyFingerprint(keyA, crypto.indexKey)
	refA := apiKeyRef(1, hashA)
	var ciphertexts []string
	for _, item := range page.Items {
		if item.APIKeyHash == hashA {
			ciphertexts = append(ciphertexts, item.APIKey)
		}
		if item.APIKey == keyA || item.APIKey == keyB {
			t.Fatal("store request detail contains plaintext API key")
		}
	}
	if len(ciphertexts) != 2 || ciphertexts[0] == ciphertexts[1] {
		t.Fatalf("same-key ciphertexts should be random: %q", ciphertexts)
	}

	storedStats, err := store.Query("24h")
	if err != nil || storedStats.Summary.Requests != 3 || len(storedStats.APIKeys) != 2 || len(storedStats.Groups) != 2 {
		t.Fatalf("stored stats = %+v, %v", storedStats, err)
	}
	for _, option := range storedStats.APIKeys {
		if option.Key == keyA || option.Key == keyB || !validAPIKeyHash(option.Hash) {
			t.Fatalf("invalid stored API-key option: %+v", option)
		}
	}

	call := func(path string, query url.Values, session string) pluginapi.ManagementResponse {
		t.Helper()
		headers := http.Header{}
		if session != "" {
			headers.Set("X-Full-Mode-Session", session)
		}
		raw, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: path, Query: query, Headers: headers})
		response, callErr := runtime.handleManagement(raw)
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}

	ordinary := call(runtime.routes.resourceStatsPath, url.Values{"range": {"24h"}}, "")
	if ordinary.StatusCode != http.StatusOK {
		t.Fatalf("ordinary stats: %+v", ordinary)
	}
	for _, forbidden := range []string{keyA, keyB, `"api_key"`, `"api_key_hash"`, `"api_key_generation"`, `"api_key_ref"`, `"api_key_status"`, `"api_keys"`} {
		if bytes.Contains(ordinary.Body, []byte(forbidden)) {
			t.Fatalf("ordinary stats leaked %q: %s", forbidden, ordinary.Body)
		}
	}

	for _, path := range []string{runtime.routes.resourceStatsInitialPath, runtime.routes.resourceStatsTrendPath, runtime.routes.resourceStatsGroupsPath} {
		query := url.Values{"range": {"24h"}}
		if path == runtime.routes.resourceStatsGroupsPath {
			query.Set("offset", "0")
			query.Set("limit", "100")
		}
		response := call(path, query, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("ordinary compact stats %s: %+v", path, response)
		}
		for _, forbidden := range []string{keyA, keyB, `"api_key"`, `"api_key_hash"`, `"api_key_generation"`, `"api_key_ref"`, `"api_key_status"`, `"api_keys"`} {
			if bytes.Contains(response.Body, []byte(forbidden)) {
				t.Fatalf("ordinary compact stats %s leaked %q: %s", path, forbidden, response.Body)
			}
		}
	}

	session, err := runtime.createFullModeSession()
	if err != nil {
		t.Fatal(err)
	}
	full := call(runtime.routes.resourceStatsPath, url.Values{"range": {"24h"}}, session)
	if full.StatusCode != http.StatusOK {
		t.Fatalf("full stats: %+v", full)
	}
	fullInitial := call(runtime.routes.resourceStatsInitialPath, url.Values{"range": {"24h"}}, session)
	var revealedInitial InitialStatsResponse
	if fullInitial.StatusCode != http.StatusOK || json.Unmarshal(fullInitial.Body, &revealedInitial) != nil || len(revealedInitial.APIKeys) != 2 || revealedInitial.APIKeys[0].Key == "" {
		t.Fatalf("full initial stats: status=%d body=%s", fullInitial.StatusCode, fullInitial.Body)
	}
	fullGroups := call(runtime.routes.resourceStatsGroupsPath, url.Values{"range": {"24h"}, "offset": {"0"}, "limit": {"100"}}, session)
	var revealedGroups GroupStatsPage
	if fullGroups.StatusCode != http.StatusOK || json.Unmarshal(fullGroups.Body, &revealedGroups) != nil || len(revealedGroups.Items) != 2 || revealedGroups.Items[0].APIKeyRef == "" || revealedGroups.Items[0].APIKey == "" {
		t.Fatalf("full groups stats: status=%d body=%s", fullGroups.StatusCode, fullGroups.Body)
	}
	var revealed StatsResponse
	if err := json.Unmarshal(full.Body, &revealed); err != nil {
		t.Fatal(err)
	}
	if revealed.Summary.Requests != 3 || len(revealed.APIKeys) != 2 {
		t.Fatalf("revealed stats = %+v", revealed)
	}
	revealedKeys := map[string]bool{}
	for _, option := range revealed.APIKeys {
		revealedKeys[option.Key] = true
		if option.Ref == "" || option.Generation == 0 || option.Status != apiKeyStatusAvailable {
			t.Fatalf("revealed option has incomplete identity/status: %+v", option)
		}
	}
	if !revealedKeys[keyA] || !revealedKeys[keyB] {
		t.Fatalf("revealed options = %+v", revealed.APIKeys)
	}

	filterQuery := url.Values{"range": {"24h"}, "api_key_ref": {refA}}
	filteredStats := call(runtime.routes.resourceStatsPath, filterQuery, session)
	var filtered StatsResponse
	if filteredStats.StatusCode != http.StatusOK || json.Unmarshal(filteredStats.Body, &filtered) != nil || filtered.Summary.Requests != 2 || len(filtered.APIKeys) != 1 || filtered.APIKeys[0].Key != keyA {
		t.Fatalf("filtered stats: status=%d body=%s", filteredStats.StatusCode, filteredStats.Body)
	}
	filteredRequests := call(runtime.routes.resourceRequestsPath, filterQuery, session)
	var filteredPage RequestPage
	if filteredRequests.StatusCode != http.StatusOK || json.Unmarshal(filteredRequests.Body, &filteredPage) != nil || filteredPage.Total != 2 {
		t.Fatalf("filtered requests: status=%d body=%s", filteredRequests.StatusCode, filteredRequests.Body)
	}
	for _, item := range filteredPage.Items {
		if item.APIKey != keyA || item.APIKeyHash != hashA || item.APIKeyRef != refA || item.APIKeyStatus != apiKeyStatusAvailable {
			t.Fatalf("filtered request was not revealed: %+v", item)
		}
	}
	filteredCosts := call(runtime.routes.resourceCostsPath, filterQuery, session)
	var costs CostResponse
	if filteredCosts.StatusCode != http.StatusOK || json.Unmarshal(filteredCosts.Body, &costs) != nil || costs.Summary.Requests != 2 {
		t.Fatalf("filtered costs: status=%d body=%s", filteredCosts.StatusCode, filteredCosts.Body)
	}

	for _, path := range []string{runtime.routes.resourceStatsPath, runtime.routes.resourceStatsInitialPath, runtime.routes.resourceStatsTrendPath, runtime.routes.resourceStatsGroupsPath, runtime.routes.resourceRequestsPath, runtime.routes.resourceCostsPath} {
		if response := call(path, filterQuery, ""); response.StatusCode != http.StatusForbidden {
			t.Fatalf("unauthorized ref filter %s status = %d", path, response.StatusCode)
		}
		if response := call(path, url.Values{"range": {"24h"}, "api_key_ref": {"INVALID"}}, session); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid ref filter %s status = %d body=%s", path, response.StatusCode, response.Body)
		}
		legacy := call(path, url.Values{"range": {"24h"}, "api_key_hash": {hashA}}, session)
		if legacy.StatusCode != http.StatusOK {
			t.Fatalf("legacy unique hash filter %s status = %d body=%s", path, legacy.StatusCode, legacy.Body)
		}
	}

	data := call(runtime.routes.fullModeDataPath, nil, session)
	if data.StatusCode != http.StatusOK || !bytes.Contains(data.Body, []byte(`"api_key_tracking_enabled":true`)) || !bytes.Contains(data.Body, []byte(`"api_key_uses_default_secret":true`)) {
		t.Fatalf("full-mode data = %s", data.Body)
	}

	backup, err := store.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(backup, []byte(keyA)) || bytes.Contains(backup, []byte(keyB)) {
		t.Fatal("database backup contains plaintext API key")
	}
}

func TestDisabledAPIKeyTrackingDropsAllKeyMaterial(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = ""
	config.SyncOnRecord = true
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{store: store, config: config, routes: registeredRoutes{pluginID: "test", fullModeDataPath: "/full-mode/data"}}
	defer runtime.shutdown()
	plain := "disabled-tracking-test-key"
	record, _ := json.Marshal(pluginapi.UsageRecord{Model: "m", APIKey: plain, RequestedAt: time.Now().UTC(), Detail: pluginapi.UsageDetail{TotalTokens: 1}})
	if _, err := runtime.handleUsage(record); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Query("24h")
	if err != nil || len(stats.APIKeys) != 0 || len(stats.Groups) != 1 || stats.Groups[0].APIKey != "" || stats.Groups[0].APIKeyHash != "" || stats.Groups[0].APIKeyStatus != "" {
		t.Fatalf("disabled tracking stats = %+v, %v", stats, err)
	}
	backup, err := store.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(backup, []byte(plain)) {
		t.Fatal("disabled tracking persisted plaintext API key")
	}
	session, _ := runtime.createFullModeSession()
	headers := http.Header{"X-Full-Mode-Session": []string{session}}
	raw, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeDataPath, Headers: headers})
	response, err := runtime.handleManagement(raw)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"api_key_tracking_enabled":false`) || !strings.Contains(string(response.Body), `"api_key_uses_default_secret":false`) {
		t.Fatalf("disabled full-mode data = %+v, %v", response, err)
	}
}

func TestEnabledTrackingMarksMissingHostAPIKeyWithoutExposingIdentity(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = defaultAPIKeySecret
	config.SyncOnRecord = true
	crypto, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStoreWithCrypto(config, crypto)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{
		store:  store,
		config: config,
		crypto: crypto,
		routes: registeredRoutes{pluginID: "test", resourceStatsPath: "/v0/resource/plugins/test/stats", resourceRequestsPath: "/v0/resource/plugins/test/requests"},
	}
	runtime.apiKeyGeneration, runtime.apiKeyGenerations = store.APIKeyCryptoState()
	defer runtime.shutdown()

	record, _ := json.Marshal(pluginapi.UsageRecord{Model: "missing-key-model", RequestedAt: time.Now().UTC(), Detail: pluginapi.UsageDetail{TotalTokens: 1}})
	if _, err := runtime.handleUsage(record); err != nil {
		t.Fatal(err)
	}
	session, err := runtime.createFullModeSession()
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, session string) pluginapi.ManagementResponse {
		headers := http.Header{}
		if session != "" {
			headers.Set("X-Full-Mode-Session", session)
		}
		raw, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: path, Query: url.Values{"range": {"24h"}}, Headers: headers})
		response, callErr := runtime.handleManagement(raw)
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}

	fullStats := call(runtime.routes.resourceStatsPath, session)
	var stats StatsResponse
	if fullStats.StatusCode != http.StatusOK || json.Unmarshal(fullStats.Body, &stats) != nil || len(stats.Groups) != 1 {
		t.Fatalf("full stats = %d %s", fullStats.StatusCode, fullStats.Body)
	}
	if stats.Groups[0].APIKeyStatus != apiKeyStatusSourceMissing || stats.Groups[0].APIKey != "" || stats.Groups[0].APIKeyRef != "" || len(stats.APIKeys) != 0 {
		t.Fatalf("missing host key was not isolated: %+v", stats)
	}
	fullRequests := call(runtime.routes.resourceRequestsPath, session)
	var page RequestPage
	if fullRequests.StatusCode != http.StatusOK || json.Unmarshal(fullRequests.Body, &page) != nil || len(page.Items) != 1 || page.Items[0].APIKeyStatus != apiKeyStatusSourceMissing {
		t.Fatalf("full requests = %d %s", fullRequests.StatusCode, fullRequests.Body)
	}
	ordinary := call(runtime.routes.resourceStatsPath, "")
	for _, forbidden := range []string{`"api_key"`, `"api_key_hash"`, `"api_key_generation"`, `"api_key_ref"`, `"api_key_status"`, apiKeyStatusSourceMissing} {
		if bytes.Contains(ordinary.Body, []byte(forbidden)) {
			t.Fatalf("ordinary response leaked %q: %s", forbidden, ordinary.Body)
		}
	}
}

func TestLegacyAPIKeyHashFilterRejectsAmbiguousGenerations(t *testing.T) {
	configA := testConfig(t)
	configA.APIKeySecret = strings.Repeat("a", 32)
	configA.SyncOnRecord = true
	ctxA, err := deriveCryptoContext(configA.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStoreWithCrypto(configA, ctxA)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sharedHash := apiKeyFingerprint("generation-a-key", ctxA.indexKey)
	if err := store.Record(encryptedUsageForGeneration(t, ctxA, 1, "generation-a-key", "generation-a", 1)); err != nil {
		t.Fatal(err)
	}
	configB := configA
	configB.APIKeySecret = strings.Repeat("b", 32)
	ctxB, err := deriveCryptoContext(configB.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReconfigureWithCrypto(configB, ctxB); err != nil {
		t.Fatal(err)
	}
	generationB, generations := store.APIKeyCryptoState()
	ciphertextB, err := encryptAPIKeyForGeneration(ctxB, "generation-b-key", sharedHash, generationB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(normalizedUsage{
		RequestedAt: time.Now().UTC(),
		Dimensions: Dimensions{
			Model:            "generation-b",
			APIKey:           ciphertextB,
			APIKeyHash:       sharedHash,
			APIKeyGeneration: generationB,
		},
		Counters: Counters{Requests: 1, TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}

	runtime := &pluginRuntime{
		store:             store,
		config:            configB,
		crypto:            ctxB,
		apiKeyGeneration:  generationB,
		apiKeyGenerations: generations,
		routes: registeredRoutes{
			pluginID:          "test",
			resourceStatsPath: "/v0/resource/plugins/test/stats",
		},
	}
	session, err := runtime.createFullModeSession()
	if err != nil {
		t.Fatal(err)
	}
	call := func(query url.Values) pluginapi.ManagementResponse {
		headers := http.Header{"X-Full-Mode-Session": []string{session}}
		raw, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.resourceStatsPath, Query: query, Headers: headers})
		response, callErr := runtime.handleManagement(raw)
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}

	ambiguous := call(url.Values{"range": {"24h"}, "api_key_hash": {sharedHash}})
	if ambiguous.StatusCode != http.StatusBadRequest || !strings.Contains(string(ambiguous.Body), "multiple crypto generations") {
		t.Fatalf("ambiguous legacy filter = %d %s", ambiguous.StatusCode, ambiguous.Body)
	}
	refB := apiKeyRef(generationB, sharedHash)
	filtered := call(url.Values{"range": {"24h"}, "api_key_ref": {refB}})
	var stats StatsResponse
	if filtered.StatusCode != http.StatusOK || json.Unmarshal(filtered.Body, &stats) != nil || stats.Summary.Requests != 1 || len(stats.Groups) != 1 || stats.Groups[0].APIKeyRef != refB {
		t.Fatalf("generation-specific filter = %d %s", filtered.StatusCode, filtered.Body)
	}
}

func BenchmarkAPIKeyPersistenceFootprint(b *testing.B) {
	for _, test := range []struct {
		name   string
		secret string
		key    string
	}{
		{name: "disabled", secret: ""},
		{name: "encrypted", secret: strings.Repeat("s", 32), key: "benchmark-client-api-key"},
	} {
		b.Run(test.name, func(b *testing.B) {
			config := Config{
				DataPath:        filepath.Join(b.TempDir(), "usage.db"),
				RetentionDays:   30,
				FlushInterval:   time.Hour,
				FlushMaxRecords: b.N + 1,
				APIKeySecret:    test.secret,
			}
			ctx, err := deriveCryptoContext(config.APIKeySecret)
			if err != nil {
				b.Fatal(err)
			}
			store, err := openStoreWithCrypto(config, ctx)
			if err != nil {
				b.Fatal(err)
			}
			generation, _ := store.APIKeyCryptoState()
			now := time.Now().UTC().Truncate(time.Minute)
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				usage := normalizedUsage{
					RequestedAt: now.Add(time.Duration(index) * time.Nanosecond),
					Dimensions:  Dimensions{Provider: "benchmark", Model: "benchmark-model", Source: "benchmark"},
					Counters:    Counters{Requests: 1, InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
				}
				if ctx.enabled {
					hash := apiKeyFingerprint(test.key, ctx.indexKey)
					ciphertext, err := encryptAPIKeyForGeneration(ctx, test.key, hash, generation)
					if err != nil {
						b.Fatal(err)
					}
					usage.Dimensions.APIKey = ciphertext
					usage.Dimensions.APIKeyHash = hash
					usage.Dimensions.APIKeyGeneration = generation
				}
				if err := store.Record(usage); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := store.Close(); err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(config.DataPath)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(info.Size())/float64(b.N), "db-bytes/op")
		})
	}
}
