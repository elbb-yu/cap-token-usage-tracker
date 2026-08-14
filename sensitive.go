package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	apiKeyCipherVersion byte = 1
	defaultAPIKeySecret      = "123456"
	apiKeyHashVersion        = "hmac-sha256-128-v1"
)

type cryptoContext struct {
	encKey            [32]byte
	indexKey          [32]byte
	keyID             string
	enabled           bool
	usesDefaultSecret bool
}

type DecryptFunc func(ciphertext, fingerprint string) (string, error)

type Sensitive interface {
	Redact()
	Reveal(decrypt DecryptFunc)
}

func deriveDomainKey(secret, domain string) [32]byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("cap-token-usage-tracker/" + domain + "/v1"))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func deriveCryptoContext(secret string) (cryptoContext, error) {
	if secret == "" {
		return cryptoContext{}, nil
	}
	if secret != defaultAPIKeySecret && len([]byte(secret)) < 32 {
		return cryptoContext{}, errors.New("api_key_secret must be empty, 123456, or at least 32 bytes")
	}
	ctx := cryptoContext{
		encKey:            deriveDomainKey(secret, "api-key-encryption"),
		indexKey:          deriveDomainKey(secret, "api-key-index"),
		enabled:           true,
		usesDefaultSecret: secret == defaultAPIKeySecret,
	}
	idInput := append([]byte("cap-token-usage-tracker/key-id/v1:"), ctx.indexKey[:]...)
	id := sha256.Sum256(idInput)
	ctx.keyID = hex.EncodeToString(id[:16])
	return ctx, nil
}

func apiKeyFingerprint(apiKey string, indexKey [32]byte) string {
	if apiKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, indexKey[:])
	_, _ = mac.Write([]byte(apiKey))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func encryptAPIKey(ctx cryptoContext, plaintext, fingerprint string) (string, error) {
	if !ctx.enabled || plaintext == "" || fingerprint == "" {
		return "", nil
	}
	block, err := aes.NewCipher(ctx.encKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	aad := []byte("api-key/v1:" + fingerprint)
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), aad)
	combined := make([]byte, 1, 1+len(nonce)+len(sealed))
	combined[0] = apiKeyCipherVersion
	combined = append(combined, nonce...)
	combined = append(combined, sealed...)
	return base64.RawURLEncoding.EncodeToString(combined), nil
}

func decryptAPIKey(ctx cryptoContext, ciphertext, fingerprint string) (string, error) {
	if !ctx.enabled || ciphertext == "" || fingerprint == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	if len(raw) < 1 || raw[0] != apiKeyCipherVersion {
		return "", errors.New("unsupported api key ciphertext version")
	}
	raw = raw[1:]
	block, err := aes.NewCipher(ctx.encKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize+gcm.Overhead() {
		return "", errors.New("ciphertext too short")
	}
	aad := []byte("api-key/v1:" + fingerprint)
	plaintext, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], aad)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (r *pluginRuntime) sensitiveJSONResponse(status int, value Sensitive, fullMode bool, crypto cryptoContext) pluginapi.ManagementResponse {
	if fullMode {
		value.Reveal(func(ciphertext, fingerprint string) (string, error) {
			return decryptAPIKey(crypto, ciphertext, fingerprint)
		})
	} else {
		value.Redact()
	}
	return jsonResponse(status, value)
}
