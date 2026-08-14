package main

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

type blockingNonceReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingNonceReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	for index := range p {
		p[index] = byte(index + 1)
	}
	return len(p), nil
}

func encryptedUsageForTest(t *testing.T, ctx cryptoContext, key, model string, tokens uint64) normalizedUsage {
	t.Helper()
	hash := apiKeyFingerprint(key, ctx.indexKey)
	ciphertext, err := encryptAPIKey(ctx, key, hash)
	if err != nil {
		t.Fatal(err)
	}
	return normalizedUsage{
		RequestedAt: time.Now().UTC().Add(-time.Second),
		Dimensions:  Dimensions{Model: model, Source: "test", APIKey: ciphertext, APIKeyHash: hash},
		Counters:    Counters{Requests: 1, TotalTokens: tokens},
	}
}

func TestAPIKeyCryptoIdentityRestartResetAndReconfigure(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = strings.Repeat("a", 32)
	config.SyncOnRecord = true
	ctx, _ := deriveCryptoContext(config.APIKeySecret)
	store, err := openStoreWithCrypto(config, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(encryptedUsageForTest(t, ctx, "identity-test-key", "original", 7)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := bolt.Open(config.DataPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if decodeUint64(meta.Get(schemaKey)) != persistenceSchemaVersion || string(meta.Get(cryptoKeyIDKey)) != ctx.keyID || string(meta.Get(apiKeyHashVersionKey)) != apiKeyHashVersion {
			t.Fatalf("crypto metadata is incomplete")
		}
		return nil
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	wrong := config
	wrong.APIKeySecret = strings.Repeat("b", 32)
	if candidate, err := openStore(wrong); err == nil {
		candidate.Close()
		t.Fatal("database opened with a different API-key secret")
	}
	disabled := config
	disabled.APIKeySecret = ""
	if candidate, err := openStore(disabled); err == nil {
		candidate.Close()
		t.Fatal("bound database opened with tracking disabled")
	}

	store, err = openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := store.Reconfigure(wrong); err != nil {
		t.Fatalf("reconfigure after reset: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(wrong)
	if err != nil {
		t.Fatalf("reopen with replacement secret: %v", err)
	}
	defer store.Close()
	stats, err := store.Query("24h")
	if err != nil || stats.Summary.Requests != 0 || len(stats.APIKeys) != 0 {
		t.Fatalf("reset state = %+v, %v", stats, err)
	}
}

func TestRestoreRejectsDifferentCryptoIdentityWithoutChangingLiveStore(t *testing.T) {
	configA := testConfig(t)
	configA.APIKeySecret = strings.Repeat("a", 32)
	configA.SyncOnRecord = true
	ctxA, _ := deriveCryptoContext(configA.APIKeySecret)
	storeA, err := openStoreWithCrypto(configA, ctxA)
	if err != nil {
		t.Fatal(err)
	}
	plainA := "restore-source-api-key"
	if err := storeA.Record(encryptedUsageForTest(t, ctxA, plainA, "source", 11)); err != nil {
		t.Fatal(err)
	}
	backup, err := storeA.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(backup, []byte(plainA)) {
		t.Fatal("source backup contains plaintext API key")
	}

	configB := testConfig(t)
	configB.APIKeySecret = strings.Repeat("b", 32)
	configB.SyncOnRecord = true
	ctxB, _ := deriveCryptoContext(configB.APIKeySecret)
	storeB, err := openStoreWithCrypto(configB, ctxB)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	if err := storeB.Record(encryptedUsageForTest(t, ctxB, "live-api-key", "live", 5)); err != nil {
		t.Fatal(err)
	}
	if err := storeB.RestoreBackup(backup); err == nil {
		t.Fatal("restore accepted a different crypto identity")
	}
	stats, err := storeB.Query("24h")
	if err != nil || stats.Summary.TotalTokens != 5 || len(stats.Groups) != 1 || stats.Groups[0].Model != "live" {
		t.Fatalf("live state changed after rejected restore: %+v, %v", stats, err)
	}
}

func TestReconfigureCryptoIdentityRollsBackWithFailedCandidateFlush(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = ""
	db, err := bolt.Open(config.DataPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := &storeActor{
		db:                   db,
		config:               config,
		data:                 make(map[aggregateKey]Counters),
		dirty:                make(map[aggregateKey]struct{}),
		modelPrices:          make(map[string]ModelPrice),
		dashboardPreferences: defaultDashboardPreferences(),
		apiKeyCiphertexts:    make(map[string]string),
		apiKeyLabels:         make(map[string]string),
	}
	if err := actor.initialize(); err != nil {
		t.Fatal(err)
	}
	defer actor.db.Close()

	now := time.Now().UTC()
	if err := actor.flush(now, true); err != nil {
		t.Fatal(err)
	}
	if err := actor.db.Update(func(tx *bolt.Tx) error {
		hours := tx.Bucket(hoursBucket)
		hour, err := hours.CreateBucketIfNotExists(encodeInt64(now.Truncate(time.Minute).Unix()))
		if err != nil {
			return err
		}
		counters, err := json.Marshal(Counters{Requests: 1})
		if err != nil {
			return err
		}
		return hour.Put([]byte("not-json"), counters)
	}); err != nil {
		t.Fatal(err)
	}

	candidate := config
	candidate.APIKeySecret = strings.Repeat("c", 32)
	candidateCrypto, err := deriveCryptoContext(candidate.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := actor.reconfigure(candidate, candidateCrypto); err == nil {
		t.Fatal("reconfigure succeeded despite candidate flush failure")
	}
	if actor.config.APIKeySecret != "" || actor.crypto.enabled {
		t.Fatalf("actor retained failed candidate config: config=%q enabled=%v", actor.config.APIKeySecret, actor.crypto.enabled)
	}
	if err := actor.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if len(meta.Get(cryptoKeyIDKey)) != 0 || len(meta.Get(apiKeyHashVersionKey)) != 0 {
			t.Fatal("failed reconfigure persisted crypto identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaSixDatabaseMigratesToCryptoIdentity(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = strings.Repeat("d", 32)
	ctx, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(config.DataPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		if err := meta.Put(schemaKey, encodeUint64(6)); err != nil {
			return err
		}
		if err := meta.Put(sinceKey, encodeInt64(time.Now().UTC().UnixNano())); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(hoursBucket); err != nil {
			return err
		}
		_, err = tx.CreateBucket(requestsBucket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openStoreWithCrypto(config, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = bolt.Open(config.DataPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if decodeUint64(meta.Get(schemaKey)) != persistenceSchemaVersion {
			t.Fatalf("schema version = %d", decodeUint64(meta.Get(schemaKey)))
		}
		if string(meta.Get(cryptoKeyIDKey)) != ctx.keyID || string(meta.Get(apiKeyHashVersionKey)) != apiKeyHashVersion {
			t.Fatal("schema-six migration did not bind the configured crypto identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRejectsAPIKeyDataWithoutCryptoIdentity(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = defaultAPIKeySecret
	db, err := bolt.Open(config.DataPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	dimensions, err := json.Marshal(Dimensions{Model: "legacy", APIKeyHash: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	counters, err := json.Marshal(Counters{Requests: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		if err := meta.Put(schemaKey, encodeUint64(6)); err != nil {
			return err
		}
		if err := meta.Put(sinceKey, encodeInt64(now.UnixNano())); err != nil {
			return err
		}
		hours, err := tx.CreateBucket(hoursBucket)
		if err != nil {
			return err
		}
		hour, err := hours.CreateBucket(encodeInt64(now.Unix()))
		if err != nil {
			return err
		}
		if err := hour.Put(dimensions, counters); err != nil {
			return err
		}
		_, err = tx.CreateBucket(requestsBucket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := openStore(config); err == nil {
		_ = store.Close()
		t.Fatal("migration accepted API-key data without crypto identity")
	}
	db, err = bolt.Open(config.DataPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if decodeUint64(meta.Get(schemaKey)) != 6 {
			t.Fatal("failed migration changed the schema version")
		}
		if len(meta.Get(cryptoKeyIDKey)) != 0 || len(meta.Get(apiKeyHashVersionKey)) != 0 {
			t.Fatal("failed migration wrote crypto identity metadata")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsIncompleteCryptoIdentityMetadata(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = defaultAPIKeySecret
	ctx, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(config.DataPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		if err := meta.Put(schemaKey, encodeUint64(persistenceSchemaVersion)); err != nil {
			return err
		}
		if err := meta.Put(sinceKey, encodeInt64(time.Now().UTC().UnixNano())); err != nil {
			return err
		}
		if err := meta.Put(cryptoKeyIDKey, []byte(ctx.keyID)); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(hoursBucket); err != nil {
			return err
		}
		_, err = tx.CreateBucket(requestsBucket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := openStoreWithCrypto(config, ctx); err == nil {
		_ = store.Close()
		t.Fatal("database opened with incomplete crypto identity metadata")
	}
}

func TestConcurrentUsagePreventsResetWindowCryptoGenerationMix(t *testing.T) {
	config := testConfig(t)
	config.APIKeySecret = strings.Repeat("e", 32)
	config.SyncOnRecord = true
	ctx, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStoreWithCrypto(config, ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &pluginRuntime{store: store, config: config, crypto: ctx}
	defer runtime.shutdown()
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}

	reader := &blockingNonceReader{entered: make(chan struct{}), release: make(chan struct{})}
	originalReader := cryptorand.Reader
	cryptorand.Reader = reader
	defer func() { cryptorand.Reader = originalReader }()

	plainKey := "concurrent-reset-window-key"
	raw, err := json.Marshal(pluginapi.UsageRecord{
		Model:       "concurrent-model",
		APIKey:      plainKey,
		RequestedAt: time.Now().UTC(),
		Detail:      pluginapi.UsageDetail{TotalTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	usageResult := make(chan error, 1)
	go func() {
		_, err := runtime.handleUsage(raw)
		usageResult <- err
	}()
	<-reader.entered

	candidate := config
	candidate.APIKeySecret = strings.Repeat("f", 32)
	reconfigureResult := make(chan error, 1)
	go func() { reconfigureResult <- runtime.applyConfig(candidate) }()
	close(reader.release)
	if err := <-usageResult; err != nil {
		t.Fatalf("usage failed: %v", err)
	}
	if err := <-reconfigureResult; err == nil {
		t.Fatal("concurrent reconfigure replaced the crypto generation used by an in-flight usage request")
	}

	page, err := store.QueryRequests("24h", 0, 10, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("request page = %+v, %v", page, err)
	}
	revealed, err := decryptAPIKey(ctx, page.Items[0].APIKey, page.Items[0].APIKeyHash)
	if err != nil || revealed != plainKey {
		t.Fatalf("persisted request was not encrypted with the active generation: %q, %v", revealed, err)
	}
	runtime.mu.RLock()
	activeSecret := runtime.config.APIKeySecret
	activeKeyID := runtime.crypto.keyID
	runtime.mu.RUnlock()
	if activeSecret != config.APIKeySecret || activeKeyID != ctx.keyID {
		t.Fatal("failed concurrent reconfigure changed the runtime crypto generation")
	}
}
