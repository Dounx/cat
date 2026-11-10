package market_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/market"
	"github.com/dounx/cat/core/pluginhost"
	"github.com/dounx/cat/core/store"
)

func pluginZip(t *testing.T, id, version string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest, _ := json.Marshal(map[string]any{"id": id, "name": id, "version": version, "protocol": 1, "command": []string{"./run"}, "capabilities": []string{"action"}})
	for name, body := range map[string][]byte{"plugin.json": manifest, "run": []byte("#!/bin/sh\n")} {
		h := &zip.FileHeader{Name: name}
		h.SetMode(0o755)
		w, _ := zw.CreateHeader(h)
		w.Write(body)
	}
	zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestMarketInstallUpdateUninstall(t *testing.T) {
	v1, v2, evil := pluginZip(t, "hello", "1.0.0"), pluginZip(t, "hello", "1.1.0"), pluginZip(t, "other", "1.0.0")
	current := v1
	var srv *httptest.Server
	index := func() []byte {
		b, _ := json.Marshal(map[string]any{"plugins": []map[string]any{
			{"id": "hello", "name": "Hello", "version": map[bool]string{true: "1.0.0", false: "1.1.0"}[bytes.Equal(current, v1)],
				"platforms": map[string]any{"any": map[string]any{"url": srv.URL + "/hello.zip", "sha256": sum(current), "format": "zip"}}},
			{"id": "tampered", "name": "Tampered", "version": "1.0.0",
				"platforms": map[string]any{"any": map[string]any{"url": srv.URL + "/hello.zip", "sha256": strings.Repeat("a", 64), "format": "zip"}}},
			{"id": "impostor", "name": "Impostor", "version": "1.0.0",
				"platforms": map[string]any{"any": map[string]any{"url": srv.URL + "/other.zip", "sha256": sum(evil), "format": "zip"}}},
			{"id": "elsewhere", "name": "Elsewhere", "version": "1.0.0", "platforms": map[string]any{"plan9/mips": map[string]any{"url": "https://x/y.zip", "sha256": sum(v1)}}},
		}})
		return b
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			w.Write(index())
		case "/hello.zip":
			w.Write(current)
		case "/other.zip":
			w.Write(evil)
		}
	}))
	defer srv.Close()

	data := t.TempDir()
	dir := filepath.Join(data, "plugins")
	st, _ := store.Open(":memory:")
	e := engine.New(st, data, nil)
	ph := pluginhost.New(e, dir)
	_ = e.Start()
	t.Cleanup(func() { e.Stop(); st.Close() })
	m := market.New(srv.URL+"/index.json", dir, ph, e)
	ctx := context.Background()

	find := func(id string) market.Listing {
		list, err := m.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range list {
			if l.ID == id {
				return l
			}
		}
		t.Fatalf("%s not listed", id)
		return market.Listing{}
	}
	if l := find("hello"); !l.Supported || l.InstalledVersion != "" {
		t.Fatalf("before install: %+v", l)
	}
	if find("elsewhere").Supported {
		t.Fatal("a plugin without an artifact for this platform must be marked unsupported")
	}

	if err := m.Install(ctx, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if man, ok := e.Plugin("hello"); !ok || man.Version != "1.0.0" {
		t.Fatalf("plugin not registered after install: %+v", man)
	}
	if find("hello").InstalledVersion != "1.0.0" {
		t.Fatal("installed version not reported")
	}

	// A checksum mismatch and a mismatched plugin id are both refused, leaving nothing behind.
	if err := m.Install(ctx, "tampered", nil); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("tampered: %v", err)
	}
	if err := m.Install(ctx, "impostor", nil); err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("impostor: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("plugin dir should hold only hello: %v", entries)
	}

	// Update in place.
	current = v2
	if err := m.Install(ctx, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if man, _ := e.Plugin("hello"); man.Version != "1.1.0" {
		t.Fatalf("after update: %s", man.Version)
	}

	// Uninstall is refused while an instance uses the plugin.
	inst, err := e.SaveInstance(store.Instance{Plugin: "hello", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall("hello"); err == nil {
		t.Fatal("uninstall should be refused while instances exist")
	}
	_ = e.DeleteInstance(inst.ID)
	if err := m.Uninstall("hello"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Plugin("hello"); ok {
		t.Fatal("plugin still registered after uninstall")
	}
}

func TestMarketNotConfigured(t *testing.T) {
	st, _ := store.Open(":memory:")
	defer st.Close()
	e := engine.New(st, t.TempDir(), nil)
	m := market.New("", t.TempDir(), pluginhost.New(e), e)
	if _, err := m.List(context.Background()); err != market.ErrNotConfigured {
		t.Fatalf("err = %v", err)
	}
}
