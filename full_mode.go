package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const fullModeSessionTTL = 15 * time.Minute

const (
	fullModeUploadTTL       = fullModeSessionTTL
	fullModeUploadChunkSize = 6000
	fullModeUploadMaxChunks = 16000
)

type fullModeSession struct {
	expiresAt time.Time
}

type fullModeUpload struct {
	sessionHash [32]byte
	expiresAt   time.Time
	chunkCount  int
	chunks      map[int]string
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

func fullModeSessionHash(raw string) ([32]byte, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 {
		return [32]byte{}, false
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(tokenBytes) != 32 {
		return [32]byte{}, false
	}
	return sha256.Sum256(tokenBytes), true
}

func (r *pluginRuntime) purgeExpiredFullModeUploads(now time.Time) {
	for id, upload := range r.fullModeUploads {
		if !now.Before(upload.expiresAt) {
			delete(r.fullModeUploads, id)
		}
	}
}

func (r *pluginRuntime) fullModeStagedPayloadResponse(request pluginapi.ManagementRequest, maxPayloadBytes int, contentType string, handler func(pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error)) (pluginapi.ManagementResponse, error) {
	session := fullModeSessionFromRequest(request)
	if !r.validFullModeSession(session) {
		return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "full-mode session is missing or expired"}), nil
	}
	sessionHash, ok := fullModeSessionHash(session)
	if !ok {
		return jsonResponse(http.StatusUnauthorized, map[string]string{"error": "full-mode session is missing or expired"}), nil
	}

	now := nowUTC()
	switch request.Query.Get("stage") {
	case "begin":
		chunkCount, err := strconv.Atoi(request.Query.Get("chunks"))
		if err != nil || chunkCount < 1 || chunkCount > fullModeUploadMaxChunks {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid full-mode upload chunk count"}), nil
		}
		var idBytes [16]byte
		if _, err := rand.Read(idBytes[:]); err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "could not create full-mode upload"}), nil
		}
		id := base64.RawURLEncoding.EncodeToString(idBytes[:])
		r.fullModeMu.Lock()
		if r.fullModeUploads == nil {
			r.fullModeUploads = make(map[string]fullModeUpload)
		}
		r.purgeExpiredFullModeUploads(now)
		r.fullModeUploads[id] = fullModeUpload{
			sessionHash: sessionHash,
			expiresAt:   now.Add(fullModeUploadTTL),
			chunkCount:  chunkCount,
			chunks:      make(map[int]string, chunkCount),
		}
		r.fullModeMu.Unlock()
		return jsonResponse(http.StatusOK, map[string]string{"upload": id}), nil
	case "chunk":
		id := request.Query.Get("upload")
		index, err := strconv.Atoi(request.Query.Get("index"))
		chunk := request.Headers.Get("X-Full-Mode-Payload")
		if id == "" || err != nil || index < 0 || len(chunk) == 0 || len(chunk) > fullModeUploadChunkSize || strings.ContainsAny(chunk, "=+/ \t\r\n") {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid full-mode upload chunk"}), nil
		}
		r.fullModeMu.Lock()
		r.purgeExpiredFullModeUploads(now)
		upload, exists := r.fullModeUploads[id]
		if !exists || subtle.ConstantTimeCompare(upload.sessionHash[:], sessionHash[:]) != 1 || index >= upload.chunkCount {
			r.fullModeMu.Unlock()
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "unknown full-mode upload"}), nil
		}
		upload.chunks[index] = chunk
		r.fullModeUploads[id] = upload
		r.fullModeMu.Unlock()
		return jsonResponse(http.StatusOK, map[string]bool{"uploaded": true}), nil
	case "commit":
		id := request.Query.Get("upload")
		if id == "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "missing full-mode upload"}), nil
		}
		r.fullModeMu.Lock()
		r.purgeExpiredFullModeUploads(now)
		upload, exists := r.fullModeUploads[id]
		if exists {
			delete(r.fullModeUploads, id)
		}
		r.fullModeMu.Unlock()
		if !exists || subtle.ConstantTimeCompare(upload.sessionHash[:], sessionHash[:]) != 1 {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "unknown full-mode upload"}), nil
		}
		var encoded strings.Builder
		for index := 0; index < upload.chunkCount; index++ {
			chunk, present := upload.chunks[index]
			if !present {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "full-mode upload is incomplete"}), nil
			}
			encoded.WriteString(chunk)
		}
		body, err := base64.RawURLEncoding.DecodeString(encoded.String())
		if err != nil || len(body) > maxPayloadBytes {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid full-mode upload payload"}), nil
		}
		request.Body = body
		request.Headers = request.Headers.Clone()
		request.Headers.Set("Content-Type", contentType)
		return handler(request)
	default:
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid full-mode upload stage"}), nil
	}
}

func (r *pluginRuntime) fullModeRestoreResponse(request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return r.fullModeStagedPayloadResponse(request, maxDatabaseBackupBytes, "application/octet-stream", r.restoreResponse)
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
