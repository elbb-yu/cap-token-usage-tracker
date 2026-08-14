package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAPIKeyLabelResourceValidationAndPersistence(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = defaultAPIKeySecret
	config.SyncOnRecord = true
	ctx, _ := deriveCryptoContext(config.APIKeySecret)
	store, err := openStoreWithCrypto(config, ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := "label-resource-test-key"
	hash := apiKeyFingerprint(key, ctx.indexKey)
	if err := store.Record(encryptedUsageForTest(t, ctx, key, "label-model", 1)); err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{
		store:  store,
		config: config,
		crypto: ctx,
		routes: registeredRoutes{
			pluginID:                 "test",
			fullModeAPIKeyLabelsPath: "/v0/resource/plugins/test/full-mode/api-key-labels",
			fullModeDataPath:         "/v0/resource/plugins/test/full-mode/data",
		},
	}
	defer runtime.shutdown()
	session, err := runtime.createFullModeSession()
	if err != nil {
		t.Fatal(err)
	}

	call := func(method string, body []byte, authorized bool) pluginapi.ManagementResponse {
		t.Helper()
		headers := http.Header{"Content-Type": []string{"application/json"}}
		if authorized {
			headers.Set("X-Full-Mode-Session", session)
		}
		raw, _ := json.Marshal(pluginapi.ManagementRequest{Method: method, Path: runtime.routes.fullModeAPIKeyLabelsPath, Headers: headers, Body: body})
		response, callErr := runtime.handleManagement(raw)
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}

	validBody, _ := json.Marshal(map[string]string{"hash": hash, "label": "Primary client"})
	if response := call(http.MethodPut, validBody, false); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
	if response := call(http.MethodPost, validBody, true); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status = %d", response.StatusCode)
	}
	for name, body := range map[string][]byte{
		"unknown field":  []byte(`{"hash":"` + hash + `","label":"x","extra":true}`),
		"trailing JSON":  []byte(`{"hash":"` + hash + `","label":"x"}{}`),
		"invalid hash":   []byte(`{"hash":"ABC","label":"x"}`),
		"uppercase hash": []byte(`{"hash":"` + strings.ToUpper(hash) + `","label":"x"}`),
		"too long":       []byte(`{"hash":"` + hash + `","label":"` + strings.Repeat("界", maxAPIKeyLabelRunes+1) + `"}`),
		"invalid utf8":   append([]byte(`{"hash":"`+hash+`","label":"`), 0xff, '"', '}'),
	} {
		t.Run(name, func(t *testing.T) {
			if response := call(http.MethodPut, body, true); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s valid_utf8=%t", response.StatusCode, response.Body, utf8.Valid(body))
			}
		})
	}
	if response := call(http.MethodPut, make([]byte, (16<<10)+1), true); response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d", response.StatusCode)
	}
	unknownBody, _ := json.Marshal(map[string]string{"hash": strings.Repeat("0", 32), "label": "unknown"})
	if response := call(http.MethodPut, unknownBody, true); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown hash status = %d body=%s", response.StatusCode, response.Body)
	}

	if response := call(http.MethodPut, validBody, true); response.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d body=%s", response.StatusCode, response.Body)
	}
	labels, err := store.APIKeyLabels()
	if err != nil || labels[hash] != "Primary client" {
		t.Fatalf("labels = %+v, %v", labels, err)
	}
	labels[hash] = "mutated by caller"
	fresh, err := store.APIKeyLabels()
	if err != nil || fresh[hash] != "Primary client" {
		t.Fatalf("actor label map was not cloned: %+v, %v", fresh, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStoreWithCrypto(config, ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.store = store
	fresh, err = store.APIKeyLabels()
	if err != nil || fresh[hash] != "Primary client" {
		t.Fatalf("reloaded labels = %+v, %v", fresh, err)
	}

	deleteBody, _ := json.Marshal(map[string]string{"hash": hash, "label": ""})
	if response := call(http.MethodPut, deleteBody, true); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", response.StatusCode, response.Body)
	}
	if response := call(http.MethodPut, deleteBody, true); response.StatusCode != http.StatusOK {
		t.Fatalf("idempotent delete status = %d body=%s", response.StatusCode, response.Body)
	}
}

func TestAPIKeyLabelCallsReturnAfterStoreClose(t *testing.T) {
	config := testConfig(t)
	store, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := store.APIKeyLabels()
		done <- err
	}()
	go func() { done <- store.SetAPIKeyLabel(strings.Repeat("0", 32), "") }()
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("closed store label call succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("closed store label call blocked")
		}
	}
}
