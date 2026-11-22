package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dounx/cat/core/engine"
)

func TestFileSecretsPlaintext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.json")
	fs := &engine.FileSecrets{Path: p}
	if err := fs.Verify(); err != nil {
		t.Fatalf("verify missing file: %v", err)
	}
	if err := fs.Set("inst1/token", "plain-val"); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok := fs.Get("inst1/token")
	if !ok || v != "plain-val" {
		t.Fatalf("get = %q, %v", v, ok)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "plain-val") {
		t.Fatalf("expected plaintext file without MasterKey, got %s", raw)
	}
	if err := fs.Delete("inst1/token"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := fs.Get("inst1/token"); ok {
		t.Fatal("deleted key still present")
	}
}

func TestFileSecretsEncryptedRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.json")
	fs := &engine.FileSecrets{Path: p, MasterKey: "my-master-passphrase"}
	if err := fs.Verify(); err != nil {
		t.Fatalf("verify missing file: %v", err)
	}
	if err := fs.Set("hook-1/secret", "super-secret-value"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := fs.Set("bot-1/token", "bot-token-xyz"); err != nil {
		t.Fatalf("set 2: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-value") || strings.Contains(string(raw), "bot-token-xyz") {
		t.Fatalf("plaintext secret leaked to disk: %s", raw)
	}
	var env struct {
		V          int    `json:"v"`
		Alg        string `json:"alg"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope json: %v (%s)", err, raw)
	}
	if env.V != 1 || env.Alg != "aes-256-gcm" || env.Nonce == "" || env.Ciphertext == "" {
		t.Fatalf("unexpected envelope: %+v", env)
	}

	// Re-open with a fresh FileSecrets instance using the same key.
	fs2 := &engine.FileSecrets{Path: p, MasterKey: "my-master-passphrase"}
	if err := fs2.Verify(); err != nil {
		t.Fatalf("verify encrypted file: %v", err)
	}
	if v, ok := fs2.Get("hook-1/secret"); !ok || v != "super-secret-value" {
		t.Fatalf("get 1 = %q, %v", v, ok)
	}
	if v, ok := fs2.Get("bot-1/token"); !ok || v != "bot-token-xyz" {
		t.Fatalf("get 2 = %q, %v", v, ok)
	}
	if err := fs2.Delete("hook-1/secret"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := fs2.Get("hook-1/secret"); ok {
		t.Fatal("deleted key still present")
	}
	if v, ok := fs2.Get("bot-1/token"); !ok || v != "bot-token-xyz" {
		t.Fatalf("remaining key lost: %q, %v", v, ok)
	}
}

func TestFileSecretsWrongAndMissingKeyRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.json")
	fs := &engine.FileSecrets{Path: p, MasterKey: "correct-key"}
	if err := fs.Set("a/k", "precious"); err != nil {
		t.Fatal(err)
	}
	origBytes, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	// Missing key must error on Verify and Set/Delete without overwriting file.
	noKey := &engine.FileSecrets{Path: p}
	if err := noKey.Verify(); err == nil || !strings.Contains(err.Error(), "CAT_MASTER_KEY") {
		t.Fatalf("expected CAT_MASTER_KEY error on Verify, got %v", err)
	}
	if err := noKey.Set("b/k", "other"); err == nil {
		t.Fatal("expected Set with missing key to fail")
	}
	if err := noKey.Delete("a/k"); err == nil {
		t.Fatal("expected Delete with missing key to fail")
	}
	if _, ok := noKey.Get("a/k"); ok {
		t.Fatal("Get with missing key should not succeed")
	}

	// Wrong key must error on Verify and Set/Delete without overwriting file.
	wrongKey := &engine.FileSecrets{Path: p, MasterKey: "wrong-key"}
	if err := wrongKey.Verify(); err == nil {
		t.Fatal("expected Verify with wrong key to fail")
	}
	if err := wrongKey.Set("b/k", "other"); err == nil {
		t.Fatal("expected Set with wrong key to fail")
	}
	if _, ok := wrongKey.Get("a/k"); ok {
		t.Fatal("Get with wrong key should not succeed")
	}

	// Original encrypted file must remain intact and decryptable.
	afterBytes, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(origBytes) {
		t.Fatal("failed operations modified the encrypted secrets file")
	}
	if v, ok := fs.Get("a/k"); !ok || v != "precious" {
		t.Fatalf("original secret lost: %q, %v", v, ok)
	}
}

func TestFileSecretsPlaintextToEncryptedMigration(t *testing.T) {
	t.Run("migrate on next write", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "secrets.json")
		plain := &engine.FileSecrets{Path: p}
		if err := plain.Set("old/key", "legacy-secret"); err != nil {
			t.Fatal(err)
		}

		enc := &engine.FileSecrets{Path: p, MasterKey: "new-master-key"}
		// Reading existing plaintext works seamlessly.
		if v, ok := enc.Get("old/key"); !ok || v != "legacy-secret" {
			t.Fatalf("read legacy plaintext = %q, %v", v, ok)
		}
		// Writing another key encrypts the whole file, preserving the old key.
		if err := enc.Set("new/key", "fresh-secret"); err != nil {
			t.Fatalf("set after migration: %v", err)
		}
		raw, _ := os.ReadFile(p)
		if strings.Contains(string(raw), "legacy-secret") || strings.Contains(string(raw), "fresh-secret") {
			t.Fatalf("file still contains plaintext after write: %s", raw)
		}
		if v, ok := enc.Get("old/key"); !ok || v != "legacy-secret" {
			t.Fatalf("old key after migration = %q, %v", v, ok)
		}
		if v, ok := enc.Get("new/key"); !ok || v != "fresh-secret" {
			t.Fatalf("new key after migration = %q, %v", v, ok)
		}
	})

	t.Run("migrate on Verify", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "secrets.json")
		plain := &engine.FileSecrets{Path: p}
		if err := plain.Set("old/key", "legacy-secret-2"); err != nil {
			t.Fatal(err)
		}

		enc := &engine.FileSecrets{Path: p, MasterKey: "verify-master-key"}
		if err := enc.Verify(); err != nil {
			t.Fatalf("verify migration: %v", err)
		}
		raw, _ := os.ReadFile(p)
		if strings.Contains(string(raw), "legacy-secret-2") {
			t.Fatalf("Verify did not encrypt legacy plaintext file: %s", raw)
		}
		if v, ok := enc.Get("old/key"); !ok || v != "legacy-secret-2" {
			t.Fatalf("old key after Verify migration = %q, %v", v, ok)
		}
	})
}
