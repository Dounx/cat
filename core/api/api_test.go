package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
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

func TestActivityPagingAndFilters(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, t.TempDir(), nil)
	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	// Seed 5 events and attach runs to #2 (ok, r1) and #4 (error, r2).
	var ids []string
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("00000000000000000000000%d", i)
		text := fmt.Sprintf("item-%d", i)
		if i == 2 {
			text = "needle-alpha-002"
		}
		if err := st.SaveEvent(&event.Event{ID: id, Source: "hook1", Plugin: "webhook", Type: "webhook.received", Payload: map[string]any{"text": text}}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = st.SaveRun(&store.Run{ID: "run1", RuleID: "r1", RuleName: "Rule 1", EventID: ids[1], Status: "ok"})
	_ = st.SaveRun(&store.Run{ID: "run2", RuleID: "r2", RuleName: "Rule 2", EventID: ids[3], Status: "error"})

	fetchPage := func(qs string) (evs []map[string]any, runs []map[string]any, hasMore bool) {
		t.Helper()
		res, err := http.Get(srv.URL + "/api/events?with_runs=1&" + qs)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			Events  []map[string]any `json:"events"`
			Runs    []map[string]any `json:"runs"`
			HasMore bool             `json:"has_more"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Events, out.Runs, out.HasMore
	}

	evs1, runs1, more1 := fetchPage("limit=2")
	if len(evs1) != 2 || !more1 || evs1[0]["id"] != ids[4] || len(runs1) != 1 || runs1[0]["id"] != "run2" {
		t.Fatalf("page 1: len=%d more=%v first=%v runs=%+v", len(evs1), more1, evs1[0]["id"], runs1)
	}
	evs2, runs2, more2 := fetchPage("limit=2&before=" + evs1[1]["id"].(string))
	if len(evs2) != 2 || !more2 || len(runs2) != 1 || runs2[0]["id"] != "run1" {
		t.Fatalf("page 2: evs=%d more=%v runs=%+v", len(evs2), more2, runs2)
	}
	evsQ, runsQ, moreQ := fetchPage("q=NEEDLE-alpha")
	if len(evsQ) != 1 || evsQ[0]["id"] != ids[1] || len(runsQ) != 1 || moreQ {
		t.Fatalf("query search: evs=%+v runs=%+v more=%v", evsQ, runsQ, moreQ)
	}
	evsF, runsF, _ := fetchPage("failed=1")
	if len(evsF) != 1 || evsF[0]["id"] != ids[3] || len(runsF) != 1 {
		t.Fatalf("failed filter: evs=%+v runs=%+v", evsF, runsF)
	}
	evsR, _, _ := fetchPage("rule_id=r1")
	if len(evsR) != 1 || evsR[0]["id"] != ids[1] {
		t.Fatalf("rule_id filter: evs=%+v", evsR)
	}
}

func TestInstanceLogsAPI(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, t.TempDir(), nil)
	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Stop)
	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	res404, err := http.Get(srv.URL + "/api/instances/nope/logs")
	if err != nil {
		t.Fatal(err)
	}
	res404.Body.Close()
	if res404.StatusCode != 404 {
		t.Fatalf("missing instance logs status = %d, want 404", res404.StatusCode)
	}

	eng.RegisterPlugin(plugin.Manifest{ID: "badp", Name: "Bad"}, func(map[string]any) (plugin.Instance, error) {
		return nil, errors.New("boom")
	})
	inst, err := eng.SaveInstance(store.Instance{Plugin: "badp", Name: "bad", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/api/instances/" + inst.ID + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("logs status = %d", res.StatusCode)
	}
	var entries []engine.LogEntry
	if err := json.NewDecoder(res.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[0].Level != "error" {
		t.Fatalf("expected error log entry, got %+v", entries)
	}
}
