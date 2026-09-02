package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestConfiguredHostAPIKeysAppearBeforeFirstUsage(t *testing.T) {
	const managementKey = "management-test-secret"
	configured := []string{
		"sk-configured-first-1111",
		"sk-configured-second-2222",
		"sk-configured-new-3333",
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+managementKey {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(hostAPIKeysResponse{APIKeys: configured})
	}))
	defer server.Close()

	runtime, store := newQuotaTestRuntime(t)
	runtime.mu.Lock()
	runtime.config.ManagementAPIURL = server.URL
	runtime.config.ManagementAPIKey = managementKey
	runtime.hostAPIKeyClient = server.Client()
	runtime.mu.Unlock()
	runtime.invalidateConfiguredAPIKeys()

	response, err := runtime.quotaStatusesResponse()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("quota statuses = %d, %v, %s", response.StatusCode, err, response.Body)
	}
	var statuses APIKeyQuotasResponse
	if err := json.Unmarshal(response.Body, &statuses); err != nil || len(statuses.Items) != len(configured) {
		t.Fatalf("configured quota items = %+v, %v", statuses.Items, err)
	}
	for _, apiKey := range configured {
		if strings.Contains(string(response.Body), apiKey) {
			t.Fatal("configured key response leaked a plaintext API key")
		}
	}

	targetID := quotaID(downstreamCallerScope(configured[2]))
	set, err := runtime.setQuotaResponse(quotaManagementRequest(t, quotaMutationRequest{ID: targetID, LimitUSD: 50}))
	if err != nil || set.StatusCode != http.StatusOK {
		t.Fatalf("set configured key quota = %d, %v, %s", set.StatusCode, err, set.Body)
	}
	quota := mustQuotaForTest(t, store, downstreamCallerScope(configured[2]))
	if quota.LimitUSD != 50 || quota.MaskedKey != maskedAPIKey(configured[2]) {
		t.Fatalf("configured key quota = %+v", quota)
	}
}

func TestConfiguredHostAPIKeyRemovalHidesKeyAndClearsQuota(t *testing.T) {
	const managementKey = "management-test-secret"
	const keyA = "sk-configured-retained-1111"
	const keyB = "sk-configured-deleted-2222"
	configured := []string{keyA, keyB}
	var configuredMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+managementKey {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		configuredMu.Lock()
		current := append([]string(nil), configured...)
		configuredMu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(hostAPIKeysResponse{APIKeys: current})
	}))
	defer server.Close()

	runtime, store := newQuotaTestRuntime(t)
	runtime.mu.Lock()
	runtime.config.ManagementAPIURL = server.URL
	runtime.config.ManagementAPIKey = managementKey
	runtime.hostAPIKeyClient = server.Client()
	runtime.mu.Unlock()
	runtime.invalidateConfiguredAPIKeys()

	for _, item := range []quotaMutationRequest{
		{APIKey: keyA, LimitUSD: 10},
		{APIKey: keyB, LimitUSD: 20},
	} {
		response, err := runtime.setQuotaResponse(quotaManagementRequest(t, item))
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("set quota = %d, %v, %s", response.StatusCode, err, response.Body)
		}
	}
	if _, err := store.SaveModelPrices(map[string]ModelPrice{"known": {Input: 1}}); err != nil {
		t.Fatal(err)
	}
	usage, err := json.Marshal(pluginapi.UsageRecord{
		Provider: "openai", Model: "known", APIKey: keyB, RequestedAt: time.Now().UTC(),
		Detail: pluginapi.UsageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handleUsage(usage); err != nil {
		t.Fatal(err)
	}

	initial, err := runtime.quotaStatusesResponse()
	if err != nil || initial.StatusCode != http.StatusOK {
		t.Fatalf("initial quota statuses = %d, %v, %s", initial.StatusCode, err, initial.Body)
	}
	var initialStatuses APIKeyQuotasResponse
	if err := json.Unmarshal(initial.Body, &initialStatuses); err != nil || len(initialStatuses.Items) != 2 {
		t.Fatalf("initial configured quota items = %+v, %v", initialStatuses.Items, err)
	}

	configuredMu.Lock()
	configured = []string{keyA}
	configuredMu.Unlock()
	runtime.invalidateConfiguredAPIKeys()

	afterDelete, err := runtime.quotaStatusesResponse()
	if err != nil || afterDelete.StatusCode != http.StatusOK {
		t.Fatalf("quota statuses after delete = %d, %v, %s", afterDelete.StatusCode, err, afterDelete.Body)
	}
	var remaining APIKeyQuotasResponse
	if err := json.Unmarshal(afterDelete.Body, &remaining); err != nil || len(remaining.Items) != 1 {
		t.Fatalf("remaining configured quota items = %+v, %v", remaining.Items, err)
	}
	if remaining.Items[0].MaskedKey != maskedAPIKey(keyA) {
		t.Fatalf("remaining configured key = %+v", remaining.Items[0])
	}
	quotas, err := store.loadAPIKeyQuotas()
	if err != nil || len(quotas) != 1 {
		t.Fatalf("reconciled quotas = %+v, %v", quotas, err)
	}
	if _, exists := quotas[downstreamCallerScope(keyB)]; exists {
		t.Fatal("deleted host key quota was not removed")
	}
	requests, err := store.QueryRequests("24h", 0, 100, "")
	if err != nil || requests.Total != 1 {
		t.Fatalf("historical requests after key delete = %+v, %v", requests, err)
	}
}

func TestManagementAPIKeySyncConfigValidation(t *testing.T) {
	for _, input := range []string{
		"management_api_url: https://example.com/v0/management/api-keys\n",
		"management_api_key: secret\n",
		"management_api_url: http://example.com/v0/management/api-keys\nmanagement_api_key: secret\n",
		"management_api_url: https://user:pass@example.com/v0/management/api-keys\nmanagement_api_key: secret\n",
	} {
		if _, err := parseConfig([]byte(input)); err == nil {
			t.Fatalf("accepted unsafe or incomplete management sync config %q", input)
		}
	}
	config, err := parseConfig([]byte("management_api_url: https://example.com/v0/management/api-keys\nmanagement_api_key: secret\n"))
	if err != nil || config.ManagementAPIURL == "" || config.ManagementAPIKey != "secret" {
		t.Fatalf("valid management sync config = %+v, %v", config, err)
	}
}
