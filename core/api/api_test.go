package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/store"
)

func TestNormalizePublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"  ":                           "",
		"https://cat.example.com":      "https://cat.example.com",
		" https://cat.example.com/ ":   "https://cat.example.com",
		"http://10.0.0.5:7788":         "http://10.0.0.5:7788",
		"https://example.com/cat/":     "https://example.com/cat",
		"https://example.com/a%20b///": "https://example.com/a%20b",
	} {
		got, err := normalizePublicURL(in)
		if err != nil || got != want {
			t.Errorf("normalizePublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"cat.example.com", "ftp://example.com", "https://", "https://example.com/?a=1", "https://example.com/#x", "https://me:pw@example.com"} {
		if got, err := normalizePublicURL(in); err == nil {
			t.Errorf("normalizePublicURL(%q) = %q; want an error", in, got)
		}
	}
}

func TestSettings(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer((&Server{Engine: engine.New(st, t.TempDir(), nil)}).Handler())
	defer srv.Close()

	do := func(method, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+"/api/settings", strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(b))
	}
	if code, body := do("GET", ""); code != 200 || body != `{"public_url":"","market_url":""}` {
		t.Fatalf("unset: %d %s", code, body)
	}
	if code, body := do("PUT", `{"public_url":"https://cat.example.com/"}`); code != 200 || body != `{"public_url":"https://cat.example.com","market_url":""}` {
		t.Fatalf("set: %d %s", code, body)
	}
	if code, body := do("GET", ""); code != 200 || body != `{"public_url":"https://cat.example.com","market_url":""}` {
		t.Fatalf("kept: %d %s", code, body)
	}
	if code, body := do("PUT", `{"market_url":"https://mirror.example.com/index.json"}`); code != 200 || body != `{"public_url":"https://cat.example.com","market_url":"https://mirror.example.com/index.json"}` {
		t.Fatalf("set market_url: %d %s", code, body)
	}
	if code, body := do("PUT", `{"market_url":"OFF"}`); code != 200 || body != `{"public_url":"https://cat.example.com","market_url":"off"}` {
		t.Fatalf("disable market_url: %d %s", code, body)
	}
	if code, _ := do("PUT", `{"market_url":"ftp://example.com/index.json"}`); code != 400 {
		t.Fatalf("bad market_url: %d", code)
	}
	if code, _ := do("PUT", `{"public_url":"cat.example.com"}`); code != 400 {
		t.Fatalf("not a URL: %d", code)
	}
	if code, body := do("PUT", `{"public_url":""}`); code != 200 || body != `{"public_url":"","market_url":"off"}` {
		t.Fatalf("cleared public_url only: %d %s", code, body)
	}
	if code, body := do("PUT", `{"market_url":""}`); code != 200 || body != `{"public_url":"","market_url":""}` {
		t.Fatalf("cleared market_url: %d %s", code, body)
	}
	if code, body := do("GET", ""); code != 200 || body != `{"public_url":"","market_url":""}` {
		t.Fatalf("after clearing: %d %s", code, body)
	}
}

func TestServeBlob(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, t.TempDir(), nil)
	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	day := filepath.Join(eng.BlobsDir(), "20261118")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(day, "pic.png"), []byte("\x89PNG\r\n\x1a\n"), 0o600)
	_ = os.WriteFile(filepath.Join(day, "bad.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o600)

	res, err := http.Get(srv.URL + "/api/blobs/20261118/pic.png")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Disposition") != "inline" {
		t.Fatalf("png headers: %d %+v", res.StatusCode, res.Header)
	}

	res2, err := http.Get(srv.URL + "/api/blobs/20261118/bad.svg")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != 200 || !strings.HasPrefix(res2.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("svg should force attachment: %d %q", res2.StatusCode, res2.Header.Get("Content-Disposition"))
	}

	res3, err := http.Get(srv.URL + "/api/blobs/../cat.db")
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	if res3.StatusCode != 404 {
		t.Fatalf("traversal should 404, got %d", res3.StatusCode)
	}
}

func TestCancelRunAPI(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, t.TempDir(), nil)
	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/runs/missing-id/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("missing run cancel status = %d, want 404", res.StatusCode)
	}
}
