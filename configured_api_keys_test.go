package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
