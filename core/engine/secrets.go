package engine

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/dounx/cat/core/store"
)

// Secrets keeps credential values out of the database. The desktop app backs
// it with the OS keychain; headless mode with a private file.
type Secrets interface {
	Get(key string) (string, bool)
	Set(key, value string) error
	Delete(key string) error
}

const (
	// secretRef is what the database holds in place of a secret value.
	secretRef = "$secret"
	// SecretMask is what the API shows in place of a secret value. Sending it
	// back unchanged keeps the stored secret.
	SecretMask = "••••••••"
)

// secretFields lists the config fields a plugin marks as format "password".
func (e *Engine) secretFields(pluginID string) []string {
	m, ok := e.Plugin(pluginID)
	if !ok {
		return nil
	}
	props, _ := m.ConfigSchema["properties"].(map[string]any)
	var out []string
	for name, raw := range props {
		if p, ok := raw.(map[string]any); ok && p["format"] == "password" {
			out = append(out, name)
		}
	}
	return out
}

func cloneConfig(rec store.Instance) store.Instance {
	cfg := make(map[string]any, len(rec.Config))
	for k, v := range rec.Config {
		cfg[k] = v
	}
	rec.Config = cfg
	return rec
}

// seal moves secret values into the secret store, leaving references behind.
func (e *Engine) seal(rec store.Instance) (store.Instance, error) {
	if e.Secrets == nil {
		return rec, nil
	}
	rec = cloneConfig(rec)
	for _, f := range e.secretFields(rec.Plugin) {
		v, _ := rec.Config[f].(string)
		switch v {
		case SecretMask, secretRef: // unchanged
			if _, ok := e.Secrets.Get(rec.ID + "/" + f); ok {
				rec.Config[f] = secretRef
			} else {
				delete(rec.Config, f)
			}
		case "":
			_ = e.Secrets.Delete(rec.ID + "/" + f)
			delete(rec.Config, f)
		default:
			if err := e.Secrets.Set(rec.ID+"/"+f, v); err != nil {
				return rec, err
			}
			rec.Config[f] = secretRef
		}
	}
	return rec, nil
}

// unseal returns the record with secret references replaced by their values.
func (e *Engine) unseal(rec store.Instance) store.Instance {
	if e.Secrets == nil {
		return rec
	}
	rec = cloneConfig(rec)
	for k, v := range rec.Config {
		if v == secretRef {
			if s, ok := e.Secrets.Get(rec.ID + "/" + k); ok {
				rec.Config[k] = s
			} else {
				delete(rec.Config, k)
			}
		}
	}
	return rec
}

// masked returns the record as the API may show it.
func (e *Engine) masked(rec store.Instance) store.Instance {
	rec = cloneConfig(rec)
	for _, f := range e.secretFields(rec.Plugin) {
		if s, _ := rec.Config[f].(string); s != "" {
			rec.Config[f] = SecretMask
		}
	}
	for k, v := range rec.Config {
		if v == secretRef {
			rec.Config[k] = SecretMask
		}
	}
	return rec
}

const (
	secretsEnvelopeVersion = 1
	secretsEnvelopeAlg     = "aes-256-gcm"
)

type encryptedSecretsEnvelope struct {
	V          int    `json:"v"`
	Alg        string `json:"alg"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// FileSecrets stores secrets in a JSON file readable only by the owner.
// When MasterKey is non-empty, the file payload is encrypted using AES-256-GCM.
type FileSecrets struct {
	Path      string
	MasterKey string
	mu        sync.Mutex
}

func deriveSecretsKey(masterKey string) [32]byte {
	return sha256.Sum256([]byte(masterKey))
}

func parseEncryptedEnvelope(raw []byte) (encryptedSecretsEnvelope, bool) {
	var env encryptedSecretsEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.V > 0 && env.Alg != "" && env.Ciphertext != "" {
		return env, true
	}
	return encryptedSecretsEnvelope{}, false
}

func encryptSecretsPayload(plain []byte, masterKey string) ([]byte, error) {
	key := deriveSecretsKey(masterKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	env := encryptedSecretsEnvelope{
		V:          secretsEnvelopeVersion,
		Alg:        secretsEnvelopeAlg,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}
	return json.Marshal(env)
}

func decryptSecretsEnvelope(env encryptedSecretsEnvelope, masterKey string) ([]byte, error) {
	if env.V != secretsEnvelopeVersion || env.Alg != secretsEnvelopeAlg {
		return nil, fmt.Errorf("unsupported encrypted secrets format (v=%d, alg=%q)", env.V, env.Alg)
	}
	if masterKey == "" {
		return nil, errors.New("secrets file is encrypted; set CAT_MASTER_KEY to unlock it")
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, fmt.Errorf("invalid secrets nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("invalid secrets ciphertext: %w", err)
	}
	key := deriveSecretsKey(masterKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("invalid secrets nonce length")
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("failed to decrypt secrets file: wrong CAT_MASTER_KEY or corrupted file")
	}
	return plain, nil
}

func (f *FileSecrets) loadLocked() (map[string]string, bool, error) {
	m := map[string]string{}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, false, nil
		}
		return nil, false, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return m, false, nil
	}
	if env, ok := parseEncryptedEnvelope(b); ok {
		plain, err := decryptSecretsEnvelope(env, f.MasterKey)
		if err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(plain, &m); err != nil {
			return nil, false, fmt.Errorf("invalid decrypted secrets payload: %w", err)
		}
		return m, false, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false, fmt.Errorf("invalid secrets file: %w", err)
	}
	return m, true, nil
}

func (f *FileSecrets) saveLocked(m map[string]string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if f.MasterKey != "" {
		b, err = encryptSecretsPayload(b, f.MasterKey)
		if err != nil {
			return err
		}
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

// Verify checks that the secrets file (if present) can be read with the
// configured MasterKey. When MasterKey is set and an existing plaintext file
// is found, it migrates the file to encrypted format immediately.
func (f *FileSecrets) Verify() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, wasPlaintext, err := f.loadLocked()
	if err != nil {
		return err
	}
	if wasPlaintext && f.MasterKey != "" {
		return f.saveLocked(m)
	}
	return nil
}

func (f *FileSecrets) Get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, _, err := f.loadLocked()
	if err != nil {
		return "", false
	}
	v, ok := m[key]
	return v, ok
}

func (f *FileSecrets) update(fn func(map[string]string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, _, err := f.loadLocked()
	if err != nil {
		return err
	}
	fn(m)
	return f.saveLocked(m)
}

func (f *FileSecrets) Set(key, value string) error {
	return f.update(func(m map[string]string) { m[key] = value })
}

func (f *FileSecrets) Delete(key string) error {
	return f.update(func(m map[string]string) { delete(m, key) })
}
