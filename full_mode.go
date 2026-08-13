package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const fullModeSessionTTL = 15 * time.Minute

type fullModeSession struct {
	expiresAt time.Time
}

func (r *pluginRuntime) createFullModeSession() (string, error) {
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", err
	}
	now := nowUTC()
	hash := sha256.Sum256(tokenBytes[:])
	r.fullModeMu.Lock()
	if r.fullModeSessions == nil {
		r.fullModeSessions = make(map[[32]byte]fullModeSession)
	}
	for key, session := range r.fullModeSessions {
		if !now.Before(session.expiresAt) {
			delete(r.fullModeSessions, key)
		}
	}
	r.fullModeSessions[hash] = fullModeSession{expiresAt: now.Add(fullModeSessionTTL)}
	r.fullModeMu.Unlock()
	return base64.RawURLEncoding.EncodeToString(tokenBytes[:]), nil
}

func (r *pluginRuntime) validFullModeSession(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 {
		return false
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(tokenBytes) != 32 {
		return false
	}
	want := sha256.Sum256(tokenBytes)
	now := nowUTC()
	r.fullModeMu.Lock()
	defer r.fullModeMu.Unlock()
	for key, session := range r.fullModeSessions {
		if !now.Before(session.expiresAt) {
			delete(r.fullModeSessions, key)
		}
	}
	for key, session := range r.fullModeSessions {
		if subtle.ConstantTimeCompare(key[:], want[:]) == 1 {
			return now.Before(session.expiresAt)
		}
	}
	return false
}

func (r *pluginRuntime) revokeFullModeSession(raw string) {
	raw = strings.TrimSpace(raw)
	tokenBytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(tokenBytes) != 32 {
		return
	}
	hash := sha256.Sum256(tokenBytes)
	r.fullModeMu.Lock()
	delete(r.fullModeSessions, hash)
	r.fullModeMu.Unlock()
}

func fullModeSessionFromRequest(request pluginapi.ManagementRequest) string {
	return request.Headers.Get("X-Full-Mode-Session")
}

func (r *pluginRuntime) fullModeSessionResponse() (pluginapi.ManagementResponse, error) {
	token, err := r.createFullModeSession()
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "could not create full-mode session"}), nil
	}
	return jsonResponse(http.StatusOK, map[string]string{"session": token}), nil
}

func (r *pluginRuntime) fullModeDataResponse(request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if !r.validFullModeSession(fullModeSessionFromRequest(request)) {
		return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "full-mode session is missing or expired"}), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{"full_mode": true, "sensitive_data": []any{}}), nil
}

func (r *pluginRuntime) revokeFullModeSessionResponse(request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	token := fullModeSessionFromRequest(request)
	if !r.validFullModeSession(token) {
		return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "full-mode session is missing or expired"}), nil
	}
	r.revokeFullModeSession(token)
	return jsonResponse(http.StatusOK, map[string]bool{"revoked": true}), nil
}
