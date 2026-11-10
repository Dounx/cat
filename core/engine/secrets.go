package engine

import (
	"encoding/json"
	"os"
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
	return rec
}

// FileSecrets stores secrets in a JSON file readable only by the owner. It is
// not encrypted: it relies on file permissions.
type FileSecrets struct {
	Path string
	mu   sync.Mutex
}

func (f *FileSecrets) load() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(f.Path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (f *FileSecrets) Get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.load()[key]
	return v, ok
}

func (f *FileSecrets) update(fn func(map[string]string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.load()
	fn(m)
	b, _ := json.Marshal(m)
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

func (f *FileSecrets) Set(key, value string) error {
	return f.update(func(m map[string]string) { m[key] = value })
}

func (f *FileSecrets) Delete(key string) error {
	return f.update(func(m map[string]string) { delete(m, key) })
}
