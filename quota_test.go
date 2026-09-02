package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func newQuotaTestRuntime(t *testing.T) (*pluginRuntime, *Store) {
	t.Helper()
	config := testConfig(t)
	config.SyncOnRecord = true
	config.APIKeySecret = defaultAPIKeySecret
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	crypto, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	generation, generations := store.APIKeyCryptoState()
	runtime := &pluginRuntime{
		store: store, config: config, crypto: crypto,
		apiKeyGeneration: generation, apiKeyGenerations: generations,
	}
	t.Cleanup(func() { _ = runtime.shutdown() })
	return runtime, store
}

func quotaManagementRequest(t *testing.T, value any) pluginapi.ManagementRequest {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return pluginapi.ManagementRequest{Body: body}
}

func TestQuotaTracksOneDownstreamKeyAndBlocksAtLimit(t *testing.T) {
	runtime, store := newQuotaTestRuntime(t)
	settings := defaultPriceSyncSettings()
	settings.Mappings = []PriceSyncMapping{{Source: "codex-auto-review", Target: "gpt-test"}}
	if _, err := store.SavePriceBook(map[string]ModelPrice{
		"gpt-test": {
			Input: 1, Output: 10, CacheRead: 0.1, CacheCreation: 1.25,
			AccountingMode: accountingModeInputIncludesCache, Source: priceSourceManual,
		},
	}, &settings); err != nil {
		t.Fatal(err)
	}

	const limitedKey = "sk-limited-test-key-123456789"
	response, err := runtime.setQuotaResponse(quotaManagementRequest(t, quotaMutationRequest{
		APIKey: limitedKey, Label: "limited", LimitUSD: 0.5,
	}))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("set quota = status %d, err %v, body %s", response.StatusCode, err, response.Body)
	}

	usage, err := json.Marshal(pluginapi.UsageRecord{
		Provider: "openai", Model: "codex-auto-review", APIKey: limitedKey,
		RequestedAt: time.Now().UTC(),
		Detail:      pluginapi.UsageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handleUsage(usage); err != nil {
		t.Fatal(err)
	}

	statusResponse, err := runtime.quotaStatusesResponse()
	if err != nil || statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("quota status = %d, %v", statusResponse.StatusCode, err)
	}
	var statuses APIKeyQuotasResponse
	if err := json.Unmarshal(statusResponse.Body, &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses.Items) != 1 {
		t.Fatalf("quota items = %+v", statuses.Items)
	}
	status := statuses.Items[0]
	if !status.Limited || !status.Blocked || status.BlockReason != "已达到最高额度" || math.Abs(status.UsedUSD-1) > 1e-12 || status.RemainingUSD != 0 || status.UnpricedRequests != 0 {
		t.Fatalf("unexpected quota status: %+v", status)
	}
	if strings.Contains(string(statusResponse.Body), limitedKey) {
		t.Fatal("public quota response leaked the plaintext API key")
	}

	intercept, err := json.Marshal(pluginapi.RequestInterceptRequest{
		Model: "codex-auto-review", Metadata: map[string]any{"caller_scope": downstreamCallerScope(limitedKey)},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := runtime.interceptRequest(intercept, true)
	if err != nil || !blocked.Terminate || blocked.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("blocked request = %+v, %v", blocked, err)
	}

	other, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		Model: "gpt-test", Metadata: map[string]any{"caller_scope": downstreamCallerScope("sk-other-key-987654321")},
	})
	allowed, err := runtime.interceptRequest(other, true)
	if err != nil || allowed.Terminate {
		t.Fatalf("unlimited key was blocked: %+v, %v", allowed, err)
	}
}

func TestUnlimitedKeyShowsHistoricalCostWithoutPlaintextLeak(t *testing.T) {
	runtime, store := newQuotaTestRuntime(t)
	if _, err := store.SaveModelPrices(map[string]ModelPrice{
		"known": {Input: 2, Output: 8, CacheRead: 0.2, Source: priceSourceManual},
	}); err != nil {
		t.Fatal(err)
	}
	const apiKey = "sk-unlimited-test-key-123456789"
	usage, err := json.Marshal(pluginapi.UsageRecord{
		Provider: "openai", Model: "known", APIKey: apiKey,
		RequestedAt: time.Now().UTC(),
		Detail: pluginapi.UsageDetail{
			InputTokens: 1_000_000, OutputTokens: 250_000, TotalTokens: 1_250_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handleUsage(usage); err != nil {
		t.Fatal(err)
	}

	response, err := runtime.quotaStatusesResponse()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("quota status = %d, %v", response.StatusCode, err)
	}
	var statuses APIKeyQuotasResponse
	if err := json.Unmarshal(response.Body, &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses.Items) != 1 {
		t.Fatalf("quota items = %+v", statuses.Items)
	}
	status := statuses.Items[0]
	if status.Limited || status.Blocked || status.Requests != 1 || status.PricedRequests != 1 || status.UnpricedRequests != 0 || math.Abs(status.UsedUSD-4) > 1e-12 {
		t.Fatalf("unexpected unlimited status: %+v", status)
	}
	if strings.Contains(string(response.Body), apiKey) {
		t.Fatal("public quota response leaked the plaintext API key")
	}
}

func TestSetQuotaForObservedKeyByOpaqueID(t *testing.T) {
	runtime, store := newQuotaTestRuntime(t)
	if _, err := store.SaveModelPrices(map[string]ModelPrice{"known": {Input: 1}}); err != nil {
		t.Fatal(err)
	}
	const apiKey = "sk-observed-test-key-123456789"
	usage, _ := json.Marshal(pluginapi.UsageRecord{
		Provider: "openai", Model: "known", APIKey: apiKey, RequestedAt: time.Now().UTC(),
		Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	if _, err := runtime.handleUsage(usage); err != nil {
		t.Fatal(err)
	}
	statusResponse, err := runtime.quotaStatusesResponse()
	if err != nil || statusResponse.StatusCode != http.StatusOK {
		t.Fatalf("quota status = %d, %v", statusResponse.StatusCode, err)
	}
	var statuses APIKeyQuotasResponse
	if err := json.Unmarshal(statusResponse.Body, &statuses); err != nil || len(statuses.Items) != 1 {
		t.Fatalf("quota items = %+v, %v", statuses.Items, err)
	}
	set, err := runtime.setQuotaResponse(quotaManagementRequest(t, quotaMutationRequest{
		ID: statuses.Items[0].ID, Label: "observed", LimitUSD: 20,
	}))
	if err != nil || set.StatusCode != http.StatusOK {
		t.Fatalf("set observed quota = %d, %v, %s", set.StatusCode, err, set.Body)
	}
	quota := mustQuotaForTest(t, store, downstreamCallerScope(apiKey))
	if quota.MaskedKey != maskedAPIKey(apiKey) || quota.Label != "observed" || quota.LimitUSD != 20 {
		t.Fatalf("observed quota = %+v", quota)
	}
	if strings.Contains(string(set.Body), apiKey) {
		t.Fatal("set response leaked the plaintext API key")
	}
}

func TestQuotaRejectsUnknownModelAndPasswordResetStartsNewWindow(t *testing.T) {
	runtime, store := newQuotaTestRuntime(t)
	if _, err := store.SaveModelPrices(map[string]ModelPrice{
		"known": {Input: 1, Output: 1, Source: priceSourceManual},
	}); err != nil {
		t.Fatal(err)
	}
	const apiKey = "sk-reset-test-key-123456789"
	set, err := runtime.setQuotaResponse(quotaManagementRequest(t, quotaMutationRequest{APIKey: apiKey, LimitUSD: 10}))
	if err != nil || set.StatusCode != http.StatusOK {
		t.Fatalf("set quota = %d, %v", set.StatusCode, err)
	}
	var setResult struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(set.Body, &setResult); err != nil || setResult.ID == "" {
		t.Fatalf("set response = %s, %v", set.Body, err)
	}

	unknown, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		Model: "future-unpriced-model", Metadata: map[string]any{"caller_scope": downstreamCallerScope(apiKey)},
	})
	denied, err := runtime.interceptRequest(unknown, true)
	if err != nil || !denied.Terminate || !strings.Contains(string(denied.ResponseBody), "尚未配置价格") {
		t.Fatalf("unknown model response = %+v, %v", denied, err)
	}

	usage, _ := json.Marshal(pluginapi.UsageRecord{
		Model: "known", APIKey: apiKey, RequestedAt: time.Now().UTC(),
		Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	if _, err := runtime.handleUsage(usage); err != nil {
		t.Fatal(err)
	}
	reset, err := runtime.resetQuotaResponse(quotaManagementRequest(t, quotaMutationRequest{ID: setResult.ID}))
	if err != nil || reset.StatusCode != http.StatusOK {
		t.Fatalf("reset quota = %d, %v, %s", reset.StatusCode, err, reset.Body)
	}
	status, err := runtime.quotaStatus(mustQuotaForTest(t, store, downstreamCallerScope(apiKey)), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if status.UsedUSD != 0 || status.RemainingUSD != 10 || status.Blocked {
		t.Fatalf("quota did not start a fresh window: %+v", status)
	}
}

func mustQuotaForTest(t *testing.T, store *Store, scope string) APIKeyQuota {
	t.Helper()
	quotas, err := store.loadAPIKeyQuotas()
	if err != nil {
		t.Fatal(err)
	}
	quota, ok := quotas[scope]
	if !ok {
		t.Fatal("quota not found")
	}
	return quota
}

func TestQuotaDashboardDoesNotEmbedSecrets(t *testing.T) {
	routes := registeredRoutes{
		dashboardPath: "/dashboard", resourceQuotasPath: "/quotas",
		quotasPath: "/manage/quotas", quotaResetPath: "/manage/quotas/reset",
	}
	response := quotaDashboardResponse(routes)
	body := string(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "API Key 费用与额度") || strings.Contains(body, "caller_scope") {
		t.Fatalf("unexpected dashboard response")
	}
}
