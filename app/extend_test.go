package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPInitializeReportsAppVersion(t *testing.T) {
	prevVersion := Version
	prevLogger := slog.Default()
	Version = "0.1.0"
	t.Cleanup(func() {
		Version = prevVersion
		slog.SetDefault(prevLogger)
	})

	a, err := New(Options{
		Addr:      "127.0.0.1:0",
		DataDir:   t.TempDir(),
		MarketURL: "off",
		UpdateURL: "off",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = a.listener.Close()
		_ = a.Store.Close()
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Result.ServerInfo.Version != "0.1.0" {
		t.Fatalf("serverInfo.version = %q, want %q", resp.Result.ServerInfo.Version, "0.1.0")
	}
}

func TestMarketUnconfiguredReturnsCleanResponse(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	a, err := New(Options{
		Addr:      "127.0.0.1:0",
		DataDir:   t.TempDir(),
		UpdateURL: "off",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = a.listener.Close()
		_ = a.Store.Close()
	})

	req := httptest.NewRequest(http.MethodGet, "/api/market", nil)
	rec := httptest.NewRecorder()
	a.Server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Configured bool   `json:"configured"`
		URL        string `json:"url"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Configured || resp.URL != "" || resp.Error != "" {
		t.Fatalf("unexpected unconfigured market response: %+v", resp)
	}
}
