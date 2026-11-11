package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeJev answers the one question of each request: a noul with the number
// found in the state, a choice with the option the state names. It keeps the
// requests it was sent, and turns the first `busy` of them away with a 529.
func fakeJev(t *testing.T, busy int) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k-1" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if busy > 0 {
			busy--
			w.WriteHeader(529)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)
		q := body["questions"].(map[string]any)["q"].(map[string]any)
		state, _ := body["state"].(string)
		ans := map[string]any{"type": q["type"]}
		if q["type"] == "noul" {
			var p float64
			_ = json.Unmarshal([]byte(state), &p)
			ans["noul"] = p
		} else {
			probs := map[string]float64{}
			for name := range q["criteria"].(map[string]any) {
				probs[name] = 0.1
			}
			ans["choice"], ans["confidence"], ans["probabilities"] = state, 0.8, probs
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"q": ans}})
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), seen...)
	}
}

func TestJevJudge(t *testing.T) {
	srv, seen := fakeJev(t, 0)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "k-1"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// yes or no: a Noul, held against the threshold
	yesNo := []any{"yes", "no"}
	for _, tc := range []struct {
		state     string
		threshold any
		want      string
	}{
		{"0.9", nil, "yes"}, {"0.4", nil, "no"}, {"0.5", nil, "yes"},
		{"0.6", 0.7, "no"}, {"0.3", "0.2", "yes"},
	} {
		p := map[string]any{"prompt": "is it urgent", "state": tc.state, "choices": yesNo}
		if tc.threshold != nil {
			p["threshold"] = tc.threshold
		}
		out, err := h.Invoke("judge", p, nil)
		if err != nil || out["choice"] != tc.want || out["model"] != "jev-1.13.0" {
			t.Errorf("%+v: %v %v", tc, out, err)
		}
	}
	first := seen()[0]
	q := first["questions"].(map[string]any)["q"].(map[string]any)
	if first["model"] != "jev-latest" || q["type"] != "noul" || q["instructions"] != "is it urgent" || q["criteria"] != nil {
		t.Fatalf("noul request: %v", first)
	}

	// one of several: a Choice, each option with what was said about it
	out, err := h.Invoke("judge", map[string]any{
		"prompt": "which team", "state": "billing", "choices": []any{"billing", "technical", "sales"},
		"descriptions": "billing: payments\n技术：not an option\nsales：pricing",
	}, nil)
	if err != nil || out["choice"] != "billing" || out["confidence"] != 0.8 || len(out["probabilities"].(map[string]any)) != 3 {
		t.Fatalf("choice: %v %v", out, err)
	}
	reqs := seen()
	q = reqs[len(reqs)-1]["questions"].(map[string]any)["q"].(map[string]any)
	crit := q["criteria"].(map[string]any)
	if q["type"] != "choice" || len(crit) != 3 || crit["billing"] != "payments" || crit["technical"] != nil || crit["sales"] != "pricing" {
		t.Fatalf("choice request: %v", q)
	}
	// used as a plain step, the choices come as text
	if out, err := h.Invoke("judge", map[string]any{"prompt": "which", "state": "乙", "choices": "甲，乙"}, nil); err != nil || out["choice"] != "乙" {
		t.Fatalf("typed choices: %v %v", out, err)
	}
	// a state that is not text goes as it is
	if _, err := h.Invoke("judge", map[string]any{"prompt": "which", "state": map[string]any{"a": "b"}, "choices": []any{"x", "y"}}, nil); err == nil ||
		!strings.Contains(err.Error(), "不在选项里") {
		t.Fatalf("an answer outside the options should fail: %v", err)
	}
	reqs = seen()
	if s, _ := reqs[len(reqs)-1]["state"].(map[string]any); s["a"] != "b" {
		t.Fatalf("object state: %v", reqs[len(reqs)-1]["state"])
	}
	for name, p := range map[string]map[string]any{
		"no question":   {"state": "x"},
		"nothing to go": {"prompt": "q"},
		"one option":    {"prompt": "q", "state": "x", "choices": []any{"only"}},
		"bad threshold": {"prompt": "q", "state": "0.5", "threshold": 1.5},
	} {
		if _, err := h.Invoke("judge", p, nil); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

func TestJevBusyAndBadKey(t *testing.T) {
	srv, seen := fakeJev(t, 2)
	p := New()
	h := cattest.New(t, p, cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "k-1"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// turned away twice, answered the third time
	if out, err := h.Invoke("judge", map[string]any{"prompt": "q", "state": "0.9"}, nil); err != nil || out["choice"] != "yes" || len(seen()) != 1 {
		t.Fatalf("after backing off: %v %v", out, err)
	}
	bad := cattest.New(t, New(), cattest.Options{Config: map[string]any{"base_url": srv.URL, "api_key": "wrong"}})
	if err := bad.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Invoke("judge", map[string]any{"prompt": "q", "state": "x"}, nil); err == nil || !strings.Contains(err.Error(), "API Key 无效") {
		t.Fatalf("err = %v", err)
	}
	if err := cattest.New(t, New(), cattest.Options{Config: map[string]any{}}).Start(); err == nil {
		t.Fatal("starting without a key should fail")
	}
}
