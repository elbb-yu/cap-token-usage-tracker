package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestFullModeSessionProtectsFullModeResources(t *testing.T) {
	config := testConfig(t)
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	crypto, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{store: store, config: config, crypto: crypto}
	defer runtime.shutdown()
	raw, err := json.Marshal(pluginapi.ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.registerManagement(raw); err != nil {
		t.Fatal(err)
	}

	dashboardRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullDashboardPath})
	response, err := runtime.handleManagement(dashboardRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full dashboard shell: %+v, %v", response, err)
	}
	if string(response.Body) == "" || containsSensitiveFullModePayload(response.Body) {
		t.Fatal("full dashboard shell must not include protected data")
	}

	dataRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeDataPath})
	response, err = runtime.handleManagement(dataRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing full-mode session response: %+v, %v", response, err)
	}
	pricesRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModePricesPath})
	response, err = runtime.handleManagement(pricesRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing full-mode session for prices response: %+v, %v", response, err)
	}

	sessionRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.fullModeSessionPath})
	response, err = runtime.handleManagement(sessionRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode session response: %+v, %v", response, err)
	}
	var payload struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil || len(payload.Session) < 40 {
		t.Fatal("full-mode session response must contain an opaque token")
	}

	validRequest, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    runtime.routes.fullModeDataPath,
		Headers: http.Header{"X-Full-Mode-Session": []string{payload.Session}},
	})
	response, err = runtime.handleManagement(validRequest)
	if err != nil || response.StatusCode != http.StatusOK || !containsSensitiveFullModePayload(response.Body) {
		t.Fatalf("valid full-mode session response: %+v, %v", response, err)
	}
	revokeRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeSessionRevokePath, Headers: http.Header{"X-Full-Mode-Session": []string{payload.Session}}})
	response, err = runtime.handleManagement(revokeRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode session revoke response: %+v, %v", response, err)
	}
	response, err = runtime.handleManagement(validRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked full-mode session response: %+v, %v", response, err)
	}

	response, err = runtime.handleManagement(sessionRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("replacement full-mode session response: %+v, %v", response, err)
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	validRequest, _ = json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    runtime.routes.fullModeDataPath,
		Headers: http.Header{"X-Full-Mode-Session": []string{payload.Session}},
	})

	token, err := base64.RawURLEncoding.DecodeString(payload.Session)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(token)
	runtime.fullModeMu.Lock()
	runtime.fullModeSessions[hash] = fullModeSession{expiresAt: time.Now().UTC().Add(-time.Second)}
	runtime.fullModeMu.Unlock()
	response, err = runtime.handleManagement(validRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired full-mode session response: %+v, %v", response, err)
	}
}

func TestFullModeDefaultSecretWarningTracksSuccessfulReconfigure(t *testing.T) {
	config := testConfig(t)
	runtime := &pluginRuntime{}
	defer runtime.shutdown()
	lifecyclePayload := func(secret string) []byte {
		configYAML := []byte("data_path: " + filepath.ToSlash(config.DataPath) + "\napi_key_secret: " + strconv.Quote(secret) + "\n")
		payload, err := json.Marshal(lifecycleRequest{ConfigYAML: configYAML, SchemaVersion: 3})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	if _, err := runtime.register(lifecyclePayload(defaultAPIKeySecret)); err != nil {
		t.Fatal(err)
	}
	registration, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/test"})
	if _, err := runtime.registerManagement(registration); err != nil {
		t.Fatal(err)
	}
	session, err := runtime.createFullModeSession()
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    runtime.routes.fullModeDataPath,
		Headers: http.Header{"X-Full-Mode-Session": []string{session}},
	})

	response, err := runtime.handleManagement(request)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"api_key_uses_default_secret":true`) {
		t.Fatalf("default-secret full-mode data = %+v, %v", response, err)
	}

	customSecret := strings.Repeat("custom-secret-", 3)
	if _, err := runtime.reconfigure(lifecyclePayload(customSecret)); err != nil {
		t.Fatal(err)
	}
	response, err = runtime.handleManagement(request)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"api_key_uses_default_secret":false`) {
		t.Fatalf("custom-secret full-mode data = %+v, %v", response, err)
	}
}

func TestFullModeStagedPriceSaveUsesGETResourceRequests(t *testing.T) {
	config := testConfig(t)
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{store: store, config: config}
	defer runtime.shutdown()

	raw, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/test"})
	if _, err := runtime.registerManagement(raw); err != nil {
		t.Fatal(err)
	}
	sessionRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.fullModeSessionPath})
	response, err := runtime.handleManagement(sessionRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode session response: %+v, %v", response, err)
	}
	var session struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(response.Body, &session); err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"prices":{"full-mode-test":{"input":1.5,"output":6}}}`))
	baseHeaders := http.Header{"X-Full-Mode-Session": []string{session.Session}}

	beginRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModePricesSavePath, Headers: baseHeaders, Query: map[string][]string{"stage": {"begin"}, "chunks": {"1"}}})
	response, err = runtime.handleManagement(beginRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode upload begin response: %+v, %v", response, err)
	}
	var upload struct {
		Upload string `json:"upload"`
	}
	if err := json.Unmarshal(response.Body, &upload); err != nil || upload.Upload == "" {
		t.Fatalf("full-mode upload begin payload: %s, %v", response.Body, err)
	}

	chunkHeaders := baseHeaders.Clone()
	chunkHeaders.Set("X-Full-Mode-Payload", payload)
	chunkRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModePricesSavePath, Headers: chunkHeaders, Query: map[string][]string{"stage": {"chunk"}, "upload": {upload.Upload}, "index": {"0"}}})
	response, err = runtime.handleManagement(chunkRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode upload chunk response: %+v, %v", response, err)
	}
	commitRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModePricesSavePath, Headers: baseHeaders, Query: map[string][]string{"stage": {"commit"}, "upload": {upload.Upload}}})
	response, err = runtime.handleManagement(commitRequest)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"full-mode-test"`) {
		t.Fatalf("full-mode upload commit response: %+v, %v", response, err)
	}
}

func TestFullModeBackupAndRestoreRequireSession(t *testing.T) {
	config := testConfig(t)
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{store: store, config: config}
	defer runtime.shutdown()

	raw, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/test"})
	if _, err := runtime.registerManagement(raw); err != nil {
		t.Fatal(err)
	}
	backupRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeBackupPath})
	response, err := runtime.handleManagement(backupRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized full-mode backup response: %+v, %v", response, err)
	}
	restoreRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeRestorePath, Query: map[string][]string{"stage": {"begin"}, "chunks": {"1"}}})
	response, err = runtime.handleManagement(restoreRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized full-mode restore response: %+v, %v", response, err)
	}

	sessionRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.fullModeSessionPath})
	response, err = runtime.handleManagement(sessionRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode session response: %+v, %v", response, err)
	}
	var session struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(response.Body, &session); err != nil || session.Session == "" {
		t.Fatalf("full-mode session payload: %s, %v", response.Body, err)
	}

	headers := http.Header{"X-Full-Mode-Session": []string{session.Session}}
	backupRequest, _ = json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeBackupPath, Headers: headers})
	response, err = runtime.handleManagement(backupRequest)
	if err != nil || response.StatusCode != http.StatusOK || response.Headers.Get("Content-Type") != "application/octet-stream" || len(response.Body) == 0 {
		t.Fatalf("authorized full-mode backup response: %+v, %v", response, err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(response.Body)
	chunkCount := (len(encoded) + fullModeUploadChunkSize - 1) / fullModeUploadChunkSize
	restoreRequest, _ = json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeRestorePath, Headers: headers, Query: map[string][]string{"stage": {"begin"}, "chunks": {strconv.Itoa(chunkCount)}}})
	response, err = runtime.handleManagement(restoreRequest)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("full-mode restore begin response: %+v, %v", response, err)
	}
	var upload struct {
		Upload string `json:"upload"`
	}
	if err := json.Unmarshal(response.Body, &upload); err != nil || upload.Upload == "" {
		t.Fatalf("full-mode restore begin payload: %s, %v", response.Body, err)
	}
	for index := 0; index < chunkCount; index++ {
		chunkHeaders := headers.Clone()
		start := index * fullModeUploadChunkSize
		end := start + fullModeUploadChunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunkHeaders.Set("X-Full-Mode-Payload", encoded[start:end])
		chunkRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeRestorePath, Headers: chunkHeaders, Query: map[string][]string{"stage": {"chunk"}, "upload": {upload.Upload}, "index": {strconv.Itoa(index)}}})
		response, err = runtime.handleManagement(chunkRequest)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("full-mode restore chunk %d response: %+v, %v", index, response, err)
		}
	}
	commitHeaders := headers.Clone()
	commitHeaders.Set("X-Confirm-Restore", "replace")
	commitRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeRestorePath, Headers: commitHeaders, Query: map[string][]string{"stage": {"commit"}, "upload": {upload.Upload}}})
	response, err = runtime.handleManagement(commitRequest)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"restored":true`) {
		t.Fatalf("full-mode restore commit response: %+v, %v", response, err)
	}
}

func TestFullModeResetRequiresSession(t *testing.T) {
	config := testConfig(t)
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{store: store, config: config}
	defer runtime.shutdown()

	raw, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/test"})
	if _, err := runtime.registerManagement(raw); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(normalizedUsage{Dimensions: Dimensions{Model: "reset-model"}, RequestedAt: nowUTC(), Counters: Counters{Requests: 1, TotalTokens: 5}}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"confirm":"reset"}`)
	headers := http.Header{"Content-Type": []string{"application/json"}}

	unauthorized, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.fullModeResetPath, Headers: headers.Clone(), Body: body})
	response, err := runtime.handleManagement(unauthorized)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing session reset response: %+v, %v", response, err)
	}

	badSessionHeaders := headers.Clone()
	badSessionHeaders.Set("X-Full-Mode-Session", "bogus-session-token")
	badSessionRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.fullModeResetPath, Headers: badSessionHeaders, Body: body})
	response, err = runtime.handleManagement(badSessionRequest)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid session reset response: %+v, %v", response, err)
	}

	getRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.fullModeResetPath})
	response, err = runtime.handleManagement(getRequest)
	if err != nil || response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method reset response: %+v, %v", response, err)
	}

	session, err := runtime.createFullModeSession()
	if err != nil {
		t.Fatal(err)
	}
	authorizedHeaders := headers.Clone()
	authorizedHeaders.Set("X-Full-Mode-Session", session)
	authorized, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.fullModeResetPath, Headers: authorizedHeaders, Body: body})
	response, err = runtime.handleManagement(authorized)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"reset":true`) {
		t.Fatalf("authorized reset response: %+v, %v", response, err)
	}

	statsRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: runtime.routes.resourceStatsPath, Query: map[string][]string{"range": {"24h"}}})
	response, err = runtime.handleManagement(statsRequest)
	if err != nil || response.StatusCode != http.StatusOK || strings.Contains(string(response.Body), "reset-model") {
		t.Fatalf("stats after reset: %+v, %v", response, err)
	}

	managementReset, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: runtime.routes.resetPath, Headers: headers.Clone(), Body: body})
	response, err = runtime.handleManagement(managementReset)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("management reset route must remain available: %+v, %v", response, err)
	}
}

func containsSensitiveFullModePayload(body []byte) bool {
	var payload struct {
		FullMode bool `json:"full_mode"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.FullMode
}
