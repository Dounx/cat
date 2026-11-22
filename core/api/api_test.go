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
	"sync"
	"testing"
	"time"

	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
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

func TestDelayAndApprovalAPI(t *testing.T) {
	origDelay := builtin.DelayTick
	builtin.DelayTick = 20 * time.Millisecond
	defer func() { builtin.DelayTick = origDelay }()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, t.TempDir(), nil)
	for _, b := range builtin.All() {
		eng.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Stop)

	var gotMu sync.Mutex
	var gotBodies []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMu.Lock()
		gotBodies = append(gotBodies, string(b))
		gotMu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	// Wait for singletons to be running.
	waitRunning := func(instID string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if v, ok := eng.Instance(instID); ok && v.Status == engine.StatusRunning {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("instance %s did not reach running", instID)
	}
	hook, err := eng.SaveInstance(store.Instance{Plugin: "webhook", Name: "Hook", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(hook.ID)
	waitRunning(builtin.DelayID)
	waitRunning(builtin.ApprovalID)
	waitRunning("http")

	_, err = eng.SaveRule(rules.Rule{
		Name:    "部署审批流",
		Enabled: true,
		Graph: &rules.Graph{
			Nodes: []rules.Node{
				{Kind: rules.KindTrigger, Step: rules.Step{ID: "t1"}, Sources: []string{hook.ID}},
				{Kind: rules.KindAction, Step: rules.Step{ID: "d1", Instance: builtin.DelayID, Action: "wait", Params: map[string]any{"seconds": 0.05, "note": "短延时"}}},
				{Kind: rules.KindAction, Step: rules.Step{ID: "a1", Instance: builtin.ApprovalID, Action: "request", Params: map[string]any{"title": "确认执行部署", "detail": "{{ payload.text }}"}}},
				{Kind: rules.KindAction, Step: rules.Step{ID: "h1", Instance: "http", Action: "request", Params: map[string]any{"url": sink.URL, "body": "approved={{ steps.a1.approved }} comment={{ steps.a1.comment }}"}}},
			},
			Edges: []rules.Edge{
				{From: "t1", To: "d1"},
				{From: "d1", To: "a1"},
				{From: "a1", To: "h1"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Trigger webhook.
	res, err := http.Post(srv.URL+"/ingress/"+hook.ID, "application/json", strings.NewReader(`{"text":"deploy v2.0"}`))
	if err != nil || res.StatusCode != http.StatusAccepted {
		t.Fatalf("ingress failed: res=%v err=%v", res, err)
	}
	res.Body.Close()

	// Wait for delay to resume into a pending approval.
	var pending builtin.ApprovalRequest
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + "/api/approvals?status=pending")
		if err != nil {
			t.Fatal(err)
		}
		var list []builtin.ApprovalRequest
		_ = json.NewDecoder(r.Body).Decode(&list)
		r.Body.Close()
		if len(list) == 1 {
			pending = list[0]
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if pending.ID == "" || pending.Title != "确认执行部署" || pending.Detail != "deploy v2.0" || pending.RuleName != "部署审批流" {
		t.Fatalf("expected pending approval, got %+v", pending)
	}

	// Invalid decision -> 400.
	badRes, _ := http.Post(srv.URL+"/api/approvals/"+pending.ID+"/decide", "application/json", strings.NewReader(`{"decision":"maybe"}`))
	badRes.Body.Close()
	if badRes.StatusCode != 400 {
		t.Fatalf("invalid decision status = %d, want 400", badRes.StatusCode)
	}

	// Missing id -> 404.
	missRes, _ := http.Post(srv.URL+"/api/approvals/no-such-id/decide", "application/json", strings.NewReader(`{"decision":"approve"}`))
	missRes.Body.Close()
	if missRes.StatusCode != 404 {
		t.Fatalf("missing approval status = %d, want 404", missRes.StatusCode)
	}

	// Approve -> 200 and downstream HTTP receives approval details.
	okRes, err := http.Post(srv.URL+"/api/approvals/"+pending.ID+"/decide", "application/json", strings.NewReader(`{"decision":"approve","comment":"LGTM"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decided builtin.ApprovalRequest
	_ = json.NewDecoder(okRes.Body).Decode(&decided)
	okRes.Body.Close()
	if okRes.StatusCode != 200 || decided.Status != builtin.ApprovalApproved || decided.Comment != "LGTM" {
		t.Fatalf("approve failed: status=%d decided=%+v", okRes.StatusCode, decided)
	}

	// Deciding again -> 409.
	againRes, _ := http.Post(srv.URL+"/api/approvals/"+pending.ID+"/decide", "application/json", strings.NewReader(`{"decision":"reject"}`))
	againRes.Body.Close()
	if againRes.StatusCode != 409 {
		t.Fatalf("second decide status = %d, want 409", againRes.StatusCode)
	}

	// Wait for resumed downstream HTTP action.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gotMu.Lock()
		n := len(gotBodies)
		gotMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	gotMu.Lock()
	defer gotMu.Unlock()
	if len(gotBodies) != 1 || gotBodies[0] != "approved=true comment=LGTM" {
		t.Fatalf("unexpected downstream body: %+v", gotBodies)
	}
}

func TestTryRuleAPI(t *testing.T) {
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

	postTry := func(body string) map[string]any {
		t.Helper()
		res, err := http.Post(srv.URL+"/api/rules/try", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Dry-run with mock event and unreachable URL succeeds without side effects or DB writes.
	out := postTry(`{
		"rule": {
			"name": "mock-alert",
			"graph": {
				"nodes": [
					{"id": "t1", "kind": "trigger", "sources": ["hook-1"]},
					{"id": "c1", "kind": "condition", "condition": "payload.level == \"high\""},
					{"id": "s1", "kind": "action", "instance": "http", "action": "request", "params": {"url": "https://example.invalid/alert", "body": "告警: {{ payload.text }}"}}
				],
				"edges": [
					{"from": "t1", "to": "c1"},
					{"from": "c1", "to": "s1"}
				]
			}
		},
		"event": {
			"source": "hook-1",
			"type": "webhook.received",
			"payload": {"level": "high", "text": "CPU 98%"}
		},
		"dry_run": true
	}`)
	if out["error"] != nil {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	runObj, _ := out["run"].(map[string]any)
	if runObj == nil || runObj["dry_run"] != true || runObj["status"] != "ok" {
		t.Fatalf("dry-run response: %+v", out)
	}
	steps := runObj["steps"].([]any)
	s0 := steps[0].(map[string]any)
	if s0["dry_run"] != true || s0["params"].(map[string]any)["body"] != "告警: CPU 98%" {
		t.Fatalf("step 0: %+v", s0)
	}
	if evs, _ := st.ListEvents(store.EventFilter{}); len(evs) != 0 {
		t.Fatalf("mock event should not be persisted, got %d", len(evs))
	}
	if runs, _ := st.ListRuns("", "", 10); len(runs) != 0 {
		t.Fatalf("dry-run should not be persisted, got %d", len(runs))
	}

	// Backward compatibility: stored event_id with dry_run.
	_ = st.SaveEvent(&event.Event{ID: "ev-stored", Source: "hook-1", Plugin: "webhook", Type: "webhook.received", Payload: map[string]any{"level": "high", "text": "from db"}})
	outStored := postTry(`{
		"rule": {
			"name": "stored-try",
			"graph": {
				"nodes": [
					{"id": "t1", "kind": "trigger", "sources": ["hook-1"]},
					{"id": "s1", "kind": "action", "instance": "http", "action": "request", "params": {"body": "{{ payload.text }}"}}
				],
				"edges": [{"from": "t1", "to": "s1"}]
			}
		},
		"event_id": "ev-stored",
		"dry_run": true
	}`)
	if outStored["run"].(map[string]any)["steps"].([]any)[0].(map[string]any)["params"].(map[string]any)["body"] != "from db" {
		t.Fatalf("stored event dry-run: %+v", outStored)
	}

	// Missing both event_id and event returns error.
	outErr := postTry(`{"rule":{"name":"x","graph":{"nodes":[],"edges":[]}}}`)
	if outErr["error"] == nil {
		t.Fatalf("expected error when neither event_id nor event supplied: %+v", outErr)
	}
}

func TestRuleStatsAPI(t *testing.T) {
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

	r1, err := eng.SaveRule(rules.Rule{
		Name:    "AI Helper",
		Enabled: true,
		Steps:   []rules.Step{{ID: "s1", Instance: "llm", Action: "chat"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := eng.SaveRule(rules.Rule{
		Name:    "Idle Rule",
		Enabled: true,
		Steps:   []rules.Step{{ID: "s1", Instance: "http", Action: "request"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	_ = st.SaveRun(&store.Run{
		ID: "run-old", RuleID: r1.ID, RuleName: r1.Name, EventID: "e0", Status: "ok",
		StartedAt: now.Add(-48 * time.Hour),
		Steps:     []store.StepResult{{ID: "s1", Output: map[string]any{"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 25}}}},
	})
	_ = st.SaveRun(&store.Run{
		ID: "run-ok", RuleID: r1.ID, RuleName: r1.Name, EventID: "e1", Status: "ok",
		StartedAt: now.Add(-15 * time.Minute),
		Steps:     []store.StepResult{{ID: "s1", Output: map[string]any{"usage": map[string]any{"prompt_tokens": 420, "completion_tokens": 185, "total_tokens": 605}}}},
	})
	_ = st.SaveRun(&store.Run{
		ID: "run-err", RuleID: r1.ID, RuleName: r1.Name, EventID: "e2", Status: "error",
		StartedAt: now.Add(-5 * time.Minute),
	})

	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	// GET /api/rules embeds 24h stats on rules that have run, omits on idle rules.
	res, err := http.Get(srv.URL + "/api/rules")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("GET /api/rules status = %d", res.StatusCode)
	}
	var list []engine.RuleView
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]engine.RuleView{}
	for _, r := range list {
		byID[r.ID] = r
	}
	got1 := byID[r1.ID].Stats
	if got1 == nil || got1.Runs24h != 2 || got1.OK24h != 1 || got1.Failed24h != 1 || got1.Tokens24h.Total != 605 || got1.TotalRuns != 3 || got1.LastStatus != "error" {
		t.Fatalf("embedded r1 stats = %+v", got1)
	}
	if byID[r2.ID].Stats != nil {
		t.Fatalf("idle rule should have nil stats, got %+v", byID[r2.ID].Stats)
	}

	// GET /api/rules/stats?hours=72 includes the 48h-old run.
	res2, err := http.Get(srv.URL + "/api/rules/stats?hours=72")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != 200 {
		t.Fatalf("GET /api/rules/stats status = %d", res2.StatusCode)
	}
	var statsMap map[string]store.RuleStats
	if err := json.NewDecoder(res2.Body).Decode(&statsMap); err != nil {
		t.Fatal(err)
	}
	s72 := statsMap[r1.ID]
	if s72.Runs24h != 3 || s72.OK24h != 2 || s72.Failed24h != 1 || s72.Tokens24h.Total != 680 {
		t.Fatalf("72h stats = %+v", s72)
	}
}

func TestBackupAPI(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, t.TempDir(), nil)
	eng.Secrets = &engine.FileSecrets{Path: filepath.Join(t.TempDir(), "secrets.json"), MasterKey: "k"}
	for _, b := range builtin.All() {
		eng.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Stop)

	hook, err := eng.SaveInstance(store.Instance{
		Plugin: "webhook", Name: "MyHook", Enabled: true,
		Config: map[string]any{"secret": "api-secret-val"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SaveRule(rules.Rule{
		Name: "R1", Enabled: true, Sources: []string{hook.ID},
		Steps: []rules.Step{{ID: "s1", Instance: "shell", Action: "run", Params: map[string]any{"command": "echo 1"}}},
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer((&Server{Engine: eng}).Handler())
	defer srv.Close()

	// Export backup.
	res, err := http.Get(srv.URL + "/api/backup")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("GET /api/backup status = %d: %s", res.StatusCode, bodyBytes)
	}
	if strings.Contains(string(bodyBytes), "api-secret-val") {
		t.Fatalf("GET /api/backup leaked secret: %s", bodyBytes)
	}
	var exported engine.WorkspaceBackup
	if err := json.Unmarshal(bodyBytes, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Format != engine.BackupFormat || len(exported.Instances) != 1 || len(exported.Rules) != 1 {
		t.Fatalf("unexpected exported bundle: %+v", exported)
	}

	// Import into a fresh server.
	st2, _ := store.Open(":memory:")
	t.Cleanup(func() { st2.Close() })
	eng2 := engine.New(st2, t.TempDir(), nil)
	eng2.Secrets = &engine.FileSecrets{Path: filepath.Join(t.TempDir(), "secrets.json")}
	for _, b := range builtin.All() {
		eng2.RegisterPlugin(b.Manifest, b.Factory)
	}
	if err := eng2.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng2.Stop)
	srv2 := httptest.NewServer((&Server{Engine: eng2}).Handler())
	defer srv2.Close()

	// Invalid backup format -> 400.
	badRes, err := http.Post(srv2.URL+"/api/backup/import", "application/json", strings.NewReader(`{"backup":{"format":"other","version":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != 400 {
		t.Fatalf("invalid backup status = %d, want 400", badRes.StatusCode)
	}

	// Valid import -> 200 with needs_secrets.
	payload, _ := json.Marshal(map[string]any{"backup": exported, "mode": "merge"})
	impRes, err := http.Post(srv2.URL+"/api/backup/import", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	var out engine.ImportResult
	if err := json.NewDecoder(impRes.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	impRes.Body.Close()
	if impRes.StatusCode != 200 || out.InstancesImported != 1 || out.RulesImported != 1 || len(out.NeedsSecrets) != 1 || out.NeedsSecrets[0] != "MyHook" {
		t.Fatalf("unexpected import response (%d): %+v", impRes.StatusCode, out)
	}
}
