package api

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	if code, body := do("GET", ""); code != 200 || body != `{"public_url":""}` {
		t.Fatalf("unset: %d %s", code, body)
	}
	if code, body := do("PUT", `{"public_url":"https://cat.example.com/"}`); code != 200 || body != `{"public_url":"https://cat.example.com"}` {
		t.Fatalf("set: %d %s", code, body)
	}
	if code, body := do("GET", ""); code != 200 || body != `{"public_url":"https://cat.example.com"}` {
		t.Fatalf("kept: %d %s", code, body)
	}
	if code, _ := do("PUT", `{"public_url":"cat.example.com"}`); code != 400 {
		t.Fatalf("not a URL: %d", code)
	}
	if code, body := do("PUT", `{"public_url":""}`); code != 200 || body != `{"public_url":""}` {
		t.Fatalf("cleared: %d %s", code, body)
	}
	if code, body := do("GET", ""); code != 200 || body != `{"public_url":""}` {
		t.Fatalf("after clearing: %d %s", code, body)
	}
}
