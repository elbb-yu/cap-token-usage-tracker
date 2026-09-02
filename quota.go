package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

var apiKeyQuotasKey = []byte("api_key_quotas")

const (
	maxAPIKeyQuotas    = 10_000
	maxQuotaLabelRunes = 120
)

// APIKeyQuota is deliberately keyed by the host's irreversible caller scope.
// Plaintext downstream API keys are never persisted.
type APIKeyQuota struct {
	ID          string    `json:"id"`
	CallerScope string    `json:"caller_scope"`
	APIKeyRef   string    `json:"api_key_ref"`
	MaskedKey   string    `json:"masked_key"`
	Label       string    `json:"label,omitempty"`
	LimitUSD    float64   `json:"limit_usd"`
	ResetAt     time.Time `json:"reset_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type APIKeyQuotaStatus struct {
	ID               string              `json:"id"`
	APIKeyRef        string              `json:"-"`
	MaskedKey        string              `json:"masked_key"`
	Label            string              `json:"label,omitempty"`
	Limited          bool                `json:"limited"`
	LimitUSD         float64             `json:"limit_usd"`
	UsedUSD          float64             `json:"used_usd"`
	RemainingUSD     float64             `json:"remaining_usd"`
	ResetAt          time.Time           `json:"reset_at,omitempty"`
	UpdatedAt        time.Time           `json:"updated_at,omitempty"`
	Requests         uint64              `json:"requests"`
	PricedRequests   uint64              `json:"priced_requests"`
	UnpricedRequests uint64              `json:"unpriced_requests"`
	MissingPrices    []MissingPriceStats `json:"missing_prices,omitempty"`
	Blocked          bool                `json:"blocked"`
	BlockReason      string              `json:"block_reason,omitempty"`
}

type APIKeyQuotasResponse struct {
	SchemaVersion uint32              `json:"schema_version"`
	GeneratedAt   time.Time           `json:"generated_at"`
	Currency      string              `json:"currency"`
	EstimateBasis string              `json:"estimate_basis"`
	Items         []APIKeyQuotaStatus `json:"items"`
}

type quotaMutationRequest struct {
	APIKey   string  `json:"api_key"`
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	LimitUSD float64 `json:"limit_usd"`
}

func downstreamCallerScope(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

func quotaID(callerScope string) string {
	if callerScope == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cap-token-usage-tracker:quota:v1\x00" + callerScope))
	return hex.EncodeToString(sum[:])
}

func maskedAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "未识别 Key"
	}
	runes := []rune(apiKey)
	if len(runes) <= 6 {
		return "••••" + string(runes[max(0, len(runes)-2):])
	}
	return string(runes[:3]) + "••••••" + string(runes[len(runes)-4:])
}

func validateQuotaRecord(quota APIKeyQuota) error {
	if len(quota.CallerScope) != 64 {
		return errors.New("quota caller scope is invalid")
	}
	if decoded, err := hex.DecodeString(quota.CallerScope); err != nil || len(decoded) != 32 {
		return errors.New("quota caller scope is invalid")
	}
	if quota.ID != quotaID(quota.CallerScope) {
		return errors.New("quota id does not match caller scope")
	}
	if _, _, ok := parseAPIKeyRef(quota.APIKeyRef); !ok {
		return errors.New("quota api key ref is invalid")
	}
	if math.IsNaN(quota.LimitUSD) || math.IsInf(quota.LimitUSD, 0) || quota.LimitUSD <= 0 || quota.LimitUSD > 1_000_000_000 {
		return errors.New("quota limit must be greater than 0 and no more than 1000000000 USD")
	}
	if !utf8.ValidString(quota.Label) || utf8.RuneCountInString(quota.Label) > maxQuotaLabelRunes {
		return fmt.Errorf("quota label must not exceed %d characters", maxQuotaLabelRunes)
	}
	if quota.ResetAt.IsZero() || quota.UpdatedAt.IsZero() {
		return errors.New("quota timestamps are required")
	}
	return nil
}

func (s *Store) loadAPIKeyQuotas() (map[string]APIKeyQuota, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed {
		return nil, errors.New("store is closed")
	}
	result := make(map[string]APIKeyQuota)
	err := s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		raw := meta.Get(apiKeyQuotasKey)
		if len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode API key quotas: %w", err)
		}
		if len(result) > maxAPIKeyQuotas {
			return fmt.Errorf("API key quotas exceed limit %d", maxAPIKeyQuotas)
		}
		for scope, quota := range result {
			if scope != quota.CallerScope {
				return errors.New("quota map key does not match caller scope")
			}
			if err := validateQuotaRecord(quota); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) saveAPIKeyQuotas(quotas map[string]APIKeyQuota) error {
	if len(quotas) > maxAPIKeyQuotas {
		return fmt.Errorf("API key quotas exceed limit %d", maxAPIKeyQuotas)
	}
	for scope, quota := range quotas {
		if scope != quota.CallerScope {
			return errors.New("quota map key does not match caller scope")
		}
		if err := validateQuotaRecord(quota); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(quotas)
	if err != nil {
		return fmt.Errorf("encode API key quotas: %w", err)
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed {
		return errors.New("store is closed")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if len(quotas) == 0 {
			return meta.Delete(apiKeyQuotasKey)
		}
		return meta.Put(apiKeyQuotasKey, encoded)
	})
}

func (r *pluginRuntime) quotaStatus(quota APIKeyQuota, now time.Time) (APIKeyQuotaStatus, error) {
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return APIKeyQuotaStatus{}, errors.New("storage is not initialized")
	}
	rangeValue := usageRange{Name: "quota", Start: quota.ResetAt.UTC(), End: now.UTC().Add(time.Nanosecond)}
	costs, err := store.queryCostsByFilter(rangeValue, newUsageFilterFromIdentities("", []string{quota.APIKeyRef}))
	if err != nil {
		return APIKeyQuotaStatus{}, err
	}
	remaining := quota.LimitUSD - costs.Summary.TotalUSD
	if remaining < 0 {
		remaining = 0
	}
	status := APIKeyQuotaStatus{
		ID:               quota.ID,
		APIKeyRef:        quota.APIKeyRef,
		MaskedKey:        quota.MaskedKey,
		Label:            quota.Label,
		Limited:          true,
		LimitUSD:         quota.LimitUSD,
		UsedUSD:          costs.Summary.TotalUSD,
		RemainingUSD:     remaining,
		ResetAt:          quota.ResetAt,
		UpdatedAt:        quota.UpdatedAt,
		Requests:         costs.Summary.Requests,
		PricedRequests:   costs.Summary.PricedRequests,
		UnpricedRequests: costs.Summary.UnpricedRequests,
		MissingPrices:    costs.MissingPrices,
	}
	if status.UnpricedRequests > 0 {
		status.Blocked = true
		status.BlockReason = "存在未定价模型，费用不完整"
	} else if status.UsedUSD >= status.LimitUSD {
		status.Blocked = true
		status.BlockReason = "已达到最高额度"
	}
	return status, nil
}

func (r *pluginRuntime) quotaStatusesResponse() (pluginapi.ManagementResponse, error) {
	r.mu.RLock()
	store := r.store
	crypto := r.crypto
	r.mu.RUnlock()
	if store == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "storage is not initialized"}), nil
	}
	quotas, err := store.loadAPIKeyQuotas()
	if err != nil {
		return jsonResponse(errorHTTPStatus(err), map[string]string{"error": err.Error()}), nil
	}
	labels, err := store.APIKeyLabels()
	if err != nil {
		return jsonResponse(errorHTTPStatus(err), map[string]string{"error": err.Error()}), nil
	}
	now := time.Now().UTC()
	items := make([]APIKeyQuotaStatus, 0, len(quotas))
	for _, quota := range quotas {
		if quota.Label == "" {
			quota.Label = labels[quota.APIKeyRef]
		}
		status, statusErr := r.quotaStatus(quota, now)
		if statusErr != nil {
			return jsonResponse(errorHTTPStatus(statusErr), map[string]string{"error": statusErr.Error()}), nil
		}
		items = append(items, status)
	}

	// Include observed, currently unlimited keys so the page can show every key
	// that has actually produced a usage record. Plaintext is only used in memory
	// to derive the same caller scope and a short masked display value.
	allRange := usageRange{Name: "all"}
	stats, err := store.queryInitialStatsByFilter(allRange, usageFilter{})
	if err == nil && crypto.enabled {
		_, generations := store.APIKeyCryptoState()
		for _, option := range stats.APIKeys {
			metadata, ok := generations[option.Generation]
			if !ok || metadata.KeyID != crypto.keyID || option.Key == "" {
				continue
			}
			plain, decryptErr := decryptAPIKeyForGeneration(crypto, option.Key, option.Hash, option.Generation)
			if decryptErr != nil || plain == "" {
				continue
			}
			scope := downstreamCallerScope(plain)
			if _, exists := quotas[scope]; exists {
				continue
			}
			ref := option.Ref
			if ref == "" {
				ref = apiKeyRef(option.Generation, option.Hash)
			}
			costs, costErr := store.queryCostsByFilter(allRange, newUsageFilterFromIdentities("", []string{ref}))
			if costErr != nil {
				return jsonResponse(errorHTTPStatus(costErr), map[string]string{"error": costErr.Error()}), nil
			}
			status := APIKeyQuotaStatus{
				ID:               quotaID(scope),
				APIKeyRef:        ref,
				MaskedKey:        maskedAPIKey(plain),
				Label:            labels[ref],
				UsedUSD:          costs.Summary.TotalUSD,
				Requests:         costs.Summary.Requests,
				PricedRequests:   costs.Summary.PricedRequests,
				UnpricedRequests: costs.Summary.UnpricedRequests,
				MissingPrices:    costs.MissingPrices,
			}
			if status.UnpricedRequests > 0 {
				status.BlockReason = "存在未定价模型，费用不完整"
			}
			items = append(items, status)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Limited != items[j].Limited {
			return items[i].Limited
		}
		if items[i].Label != items[j].Label {
			return items[i].Label < items[j].Label
		}
		return items[i].MaskedKey < items[j].MaskedKey
	})
	return jsonResponse(http.StatusOK, APIKeyQuotasResponse{
		SchemaVersion: 1,
		GeneratedAt:   now,
		Currency:      "USD",
		EstimateBasis: "current_price_book",
		Items:         items,
	}), nil
}

func (r *pluginRuntime) setQuotaResponse(request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	r.quotaMu.Lock()
	defer r.quotaMu.Unlock()
	var input quotaMutationRequest
	if err := json.Unmarshal(request.Body, &input); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid quota request"}), nil
	}
	input.APIKey = strings.TrimSpace(input.APIKey)
	input.ID = strings.TrimSpace(input.ID)
	input.Label = strings.TrimSpace(input.Label)
	if math.IsNaN(input.LimitUSD) || math.IsInf(input.LimitUSD, 0) || input.LimitUSD <= 0 || input.LimitUSD > 1_000_000_000 {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "limit_usd must be greater than 0 and no more than 1000000000"}), nil
	}
	if !utf8.ValidString(input.Label) || utf8.RuneCountInString(input.Label) > maxQuotaLabelRunes {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "label is too long"}), nil
	}
	r.mu.RLock()
	store := r.store
	crypto := r.crypto
	generation := r.apiKeyGeneration
	r.mu.RUnlock()
	if store == nil || !crypto.enabled || generation == 0 {
		return jsonResponse(http.StatusConflict, map[string]string{"error": "API key tracking must be enabled before configuring quotas"}), nil
	}
	quotas, err := store.loadAPIKeyQuotas()
	if err != nil {
		return jsonResponse(errorHTTPStatus(err), map[string]string{"error": err.Error()}), nil
	}
	now := time.Now().UTC()
	var quota APIKeyQuota
	if input.ID != "" {
		for _, candidate := range quotas {
			if candidate.ID == input.ID {
				quota = candidate
				break
			}
		}
		if quota.ID == "" {
			return jsonResponse(http.StatusNotFound, map[string]string{"error": "quota not found"}), nil
		}
	} else {
		if input.APIKey == "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "api_key is required for a new quota"}), nil
		}
		scope := downstreamCallerScope(input.APIKey)
		fingerprint := apiKeyFingerprint(input.APIKey, crypto.indexKey)
		ref := apiKeyRef(generation, fingerprint)
		quota = APIKeyQuota{
			ID: quotaID(scope), CallerScope: scope, APIKeyRef: ref,
			MaskedKey: maskedAPIKey(input.APIKey), ResetAt: now,
		}
		if existing, exists := quotas[scope]; exists {
			quota.ResetAt = existing.ResetAt
		}
	}
	quota.Label = input.Label
	quota.LimitUSD = input.LimitUSD
	quota.UpdatedAt = now
	quotas[quota.CallerScope] = quota
	if err := store.saveAPIKeyQuotas(quotas); err != nil {
		return jsonResponse(errorHTTPStatus(err), map[string]string{"error": err.Error()}), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "id": quota.ID}), nil
}

func (r *pluginRuntime) resetQuotaResponse(request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	r.quotaMu.Lock()
	defer r.quotaMu.Unlock()
	var input quotaMutationRequest
	if err := json.Unmarshal(request.Body, &input); err != nil || strings.TrimSpace(input.ID) == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "quota id is required"}), nil
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "storage is not initialized"}), nil
	}
	quotas, err := store.loadAPIKeyQuotas()
	if err != nil {
		return jsonResponse(errorHTTPStatus(err), map[string]string{"error": err.Error()}), nil
	}
	found := false
	for scope, quota := range quotas {
		if quota.ID != strings.TrimSpace(input.ID) {
			continue
		}
		now := time.Now().UTC()
		quota.ResetAt = now
		quota.UpdatedAt = now
		quotas[scope] = quota
		found = true
		break
	}
	if !found {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "quota not found"}), nil
	}
	if err := store.saveAPIKeyQuotas(quotas); err != nil {
		return jsonResponse(errorHTTPStatus(err), map[string]string{"error": err.Error()}), nil
	}
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true}), nil
}

func quotaDeniedResponse(reason string, status *APIKeyQuotaStatus) pluginapi.RequestInterceptResponse {
	payload := map[string]any{
		"error": map[string]any{
			"type":    "api_key_quota_exceeded",
			"message": reason,
		},
	}
	if status != nil {
		payload["quota"] = map[string]any{
			"limit_usd":     status.LimitUSD,
			"used_usd":      status.UsedUSD,
			"remaining_usd": status.RemainingUSD,
		}
	}
	body, _ := json.Marshal(payload)
	return pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusPaymentRequired,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		ResponseBody:    body,
	}
}

func (r *pluginRuntime) interceptRequest(raw []byte, beforeAuth bool) (pluginapi.RequestInterceptResponse, error) {
	var request pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return pluginapi.RequestInterceptResponse{}, withStatus(http.StatusBadRequest, "decode request interceptor: %v", err)
	}
	if !beforeAuth {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	scope, _ := request.Metadata["caller_scope"].(string)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return pluginapi.RequestInterceptResponse{}, withStatus(http.StatusServiceUnavailable, "storage is not initialized")
	}
	quotas, err := store.loadAPIKeyQuotas()
	if err != nil {
		return pluginapi.RequestInterceptResponse{}, err
	}
	quota, limited := quotas[scope]
	if !limited {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	priceBook, err := store.QueryPriceBook()
	if err != nil {
		return pluginapi.RequestInterceptResponse{}, err
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = strings.TrimSpace(request.RequestedModel)
	}
	resolver := newModelPriceResolver(priceBook.Prices, priceBook.SyncSettings)
	if _, priced := resolver.resolve(model); !priced {
		return quotaDeniedResponse("该模型尚未配置价格；为防止绕过额度，有限额的 API key 已暂停调用", nil), nil
	}
	status, err := r.quotaStatus(quota, time.Now().UTC())
	if err != nil {
		return pluginapi.RequestInterceptResponse{}, err
	}
	if status.Blocked {
		return quotaDeniedResponse(status.BlockReason, &status), nil
	}
	return pluginapi.RequestInterceptResponse{}, nil
}
