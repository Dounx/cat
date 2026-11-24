package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The hub's table comes out of a config.toml as it went in, and nothing else
// in the file changes on the way.
func TestTomlCut(t *testing.T) {
	const before = "model = \"x\"\n\n[mcp_servers.serena]\ncommand = \"serena\"\ntype = \"stdio\"\n"
	const table = "[mcp_servers.cat]\nurl = \"http://127.0.0.1:7788/api/mcp\"\n"
	if _, _, ok := tomlCut(before, "mcp_servers.cat"); ok {
		t.Fatal("found a table that is not there")
	}
	for _, doc := range []string{
		before + "\n" + table, // as installed
		"model = \"x\"\n\n" + table + "\n[mcp_servers.serena]\ncommand = \"serena\"\ntype = \"stdio\"\n",                                     // moved up by a rewrite
		before + "\n[mcp_servers.cat]\nenabled = true\nurl = 'http://127.0.0.1:7788/api/mcp'\n\n[mcp_servers.cat.http_headers]\nA = \"b\"\n", // with more under it
	} {
		rest, url, ok := tomlCut(doc, "mcp_servers.cat")
		if !ok || url != "http://127.0.0.1:7788/api/mcp" || rest != before {
			t.Errorf("cut %q:\n got %q, url %q, found %v", doc, rest, url, ok)
		}
	}
	if rest, _, ok := tomlCut(table, "mcp_servers.cat"); !ok || rest != "" {
		t.Errorf("a file with nothing else: %q", rest)
	}
}

// The hub's entry comes out of a DeepSeek Harness patch file, also after the
// Harness wrote the file anew and its comments went.
func TestDshCut(t *testing.T) {
	const before = "# a patch layer\n- id: llm\n  config:\n    url: http://other\n- insert: # magpie\n    - id: magpie-mcp-context7\n      config:\n        url: \"https://mcp.context7.com/mcp\"\n"
	const entry = "- insert: # cat: added by Cat\n    - id: cat-mcp\n      name: \"@deepseek-ai/dsh-mcp-client\"\n      config:\n        serverName: \"cat\"\n        url: \"http://127.0.0.1:7788/api/mcp\"\n"
	const rewritten = "- insert:\n    - id: cat-mcp\n      name: '@deepseek-ai/dsh-mcp-client'\n      config:\n        serverName: cat\n        url: http://127.0.0.1:7788/api/mcp\n"
	if _, _, ok := dshCut(before); ok {
		t.Fatal("found an entry that is not there")
	}
	for _, doc := range []string{before + entry, before + rewritten, entry + before, "- id: llm\n" + rewritten + "\n- id: other\n"} {
		rest, url, ok := dshCut(doc)
		if !ok || url != "http://127.0.0.1:7788/api/mcp" {
			t.Fatalf("cut %q: url %q, found %v", doc, url, ok)
		}
		if _, _, again := dshCut(rest); again {
			t.Errorf("still there after the cut: %q", rest)
		}
	}
	if rest, _, _ := dshCut(before + entry); rest != before {
		t.Errorf("what was appended does not come off clean: %q", rest)
	}
	if rest, _, _ := dshCut("- id: llm\n" + rewritten + "\n- id: other\n"); rest != "- id: llm\n\n- id: other\n" {
		t.Errorf("an entry in the middle: %q", rest)
	}
}

func TestJSONHarnessRoundTrip(t *testing.T) {
	cases := []struct {
		id     string
		envKey string
		relCfg string // empty when envKey points straight to the file
		urlKey string
		makeH  func() Harness
	}{
		{id: "cursor", envKey: "CURSOR_CONFIG_DIR", relCfg: "mcp.json", urlKey: "url", makeH: cursor},
		{id: "windsurf", envKey: "WINDSURF_CONFIG_DIR", relCfg: "mcp_config.json", urlKey: "serverUrl", makeH: windsurf},
		{id: "claude-app", envKey: "CLAUDE_DESKTOP_CONFIG", relCfg: "", urlKey: "url", makeH: claudeApp},
	}

	const url1 = "http://127.0.0.1:7788/api/mcp"
	const url2 = "http://127.0.0.1:17785/api/mcp?token=abc"

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			base := t.TempDir()
			missingDir := filepath.Join(base, "missing")
			var cfgPath string
			if tc.relCfg == "" {
				cfgPath = filepath.Join(missingDir, "claude_desktop_config.json")
				t.Setenv(tc.envKey, cfgPath)
			} else {
				cfgPath = filepath.Join(missingDir, tc.relCfg)
				t.Setenv(tc.envKey, missingDir)
			}

			// Not found when config dir does not exist
			st := StatusOf(tc.makeH(), url1)
			if st.Found || st.Installed {
				t.Fatalf("expected not found, got %+v", st)
			}

			// Create directory with pre-existing unrelated settings and another MCP server
			if err := os.MkdirAll(missingDir, 0o700); err != nil {
				t.Fatal(err)
			}
			const existing = `{
  "theme": "dark",
  "preferences": { "sidebar": true },
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    }
  }
}
`
			if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
				t.Fatal(err)
			}

			st = StatusOf(tc.makeH(), url1)
			if !st.Found || st.Installed || st.Stale {
				t.Fatalf("expected found & not installed, got %+v", st)
			}

			// Install url1
			ctx := context.Background()
			if err := Install(ctx, tc.id, url1); err != nil {
				t.Fatalf("Install(%s): %v", tc.id, err)
			}
			st = StatusOf(tc.makeH(), url1)
			if !st.Found || !st.Installed || st.Stale || st.URL != url1 {
				t.Fatalf("after install: %+v", st)
			}

			// Verify JSON preserves existing keys & sibling server, and writes the right urlKey
			assertPreserved := func(expectCatURL string) {
				t.Helper()
				raw, err := os.ReadFile(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				var doc map[string]any
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("invalid json: %v\n%s", err, raw)
				}
				if doc["theme"] != "dark" {
					t.Errorf("theme lost: %+v", doc)
				}
				prefs, _ := doc["preferences"].(map[string]any)
				if prefs["sidebar"] != true {
					t.Errorf("preferences lost: %+v", doc)
				}
				servers, _ := doc["mcpServers"].(map[string]any)
				if servers["filesystem"] == nil {
					t.Errorf("sibling server filesystem clobbered: %+v", servers)
				}
				if expectCatURL == "" {
					if _, exists := servers[ServerName]; exists {
						t.Errorf("cat entry should have been removed: %+v", servers)
					}
				} else {
					catEntry, _ := servers[ServerName].(map[string]any)
					if catEntry[tc.urlKey] != expectCatURL {
						t.Errorf("expected %s=%q, got %+v", tc.urlKey, expectCatURL, catEntry)
					}
				}
			}
			assertPreserved(url1)

			// Stale detection against url2
			st = StatusOf(tc.makeH(), url2)
			if !st.Installed || !st.Stale || st.URL != url1 {
				t.Fatalf("expected stale against url2, got %+v", st)
			}

			// Reinstall updates to url2 without losing anything
			if err := Install(ctx, tc.id, url2); err != nil {
				t.Fatalf("reinstall: %v", err)
			}
			st = StatusOf(tc.makeH(), url2)
			if !st.Installed || st.Stale || st.URL != url2 {
				t.Fatalf("after reinstall: %+v", st)
			}
			assertPreserved(url2)

			// Uninstall removes cat while preserving sibling server & settings
			if err := Uninstall(ctx, tc.id); err != nil {
				t.Fatalf("Uninstall(%s): %v", tc.id, err)
			}
			st = StatusOf(tc.makeH(), url2)
			if st.Installed || st.Stale || st.URL != "" {
				t.Fatalf("after uninstall: %+v", st)
			}
			assertPreserved("")
		})
	}
}

func TestWindsurfReadsLegacyURLKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WINDSURF_CONFIG_DIR", dir)
	cfg := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"cat":{"url":"http://127.0.0.1:7788/api/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st := StatusOf(windsurf(), "http://127.0.0.1:7788/api/mcp")
	if !st.Found || !st.Installed || st.Stale {
		t.Fatalf("expected installed from legacy url key, got %+v", st)
	}
}

func TestBatchInstallAndUninstall(t *testing.T) {
	base := t.TempDir()
	cursorDir := filepath.Join(base, "cursor")
	windsurfDir := filepath.Join(base, "windsurf")
	missingDir := filepath.Join(base, "claude-missing")
	if err := os.MkdirAll(cursorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(windsurfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_CONFIG_DIR", cursorDir)
	t.Setenv("WINDSURF_CONFIG_DIR", windsurfDir)
	t.Setenv("CLAUDE_DESKTOP_CONFIG", filepath.Join(missingDir, "claude_desktop_config.json"))

	// Seed windsurf with a stale address so InstallAll should update it alongside fresh Cursor install.
	if err := os.WriteFile(
		filepath.Join(windsurfDir, "mcp_config.json"),
		[]byte(`{"mcpServers":{"cat":{"serverUrl":"http://127.0.0.1:9999/api/mcp"}}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	broken := Harness{
		ID:        "broken",
		Name:      "Broken App",
		find:      func() (string, bool) { return "/tmp/broken", true },
		installed: func() (string, bool) { return "", false },
		install:   func(context.Context, string, string) error { return errors.New("boom") },
		uninstall: func(context.Context, string) error { return nil },
	}

	const url = "http://127.0.0.1:7788/api/mcp"
	harnesses := []Harness{cursor(), windsurf(), claudeApp(), broken}

	res := installAll(context.Background(), harnesses, url)
	if len(res.Changed) != 2 {
		t.Fatalf("expected 2 installed/updated harnesses, got %+v", res)
	}
	if len(res.Failed) != 1 || res.Failed[0].ID != "broken" {
		t.Fatalf("expected 1 failure for broken, got %+v", res.Failed)
	}
	if st := StatusOf(cursor(), url); !st.Installed || st.Stale {
		t.Fatalf("cursor not installed cleanly: %+v", st)
	}
	if st := StatusOf(windsurf(), url); !st.Installed || st.Stale || st.URL != url {
		t.Fatalf("windsurf not updated cleanly: %+v", st)
	}
	if st := StatusOf(claudeApp(), url); st.Installed {
		t.Fatalf("missing claude-app should have been skipped: %+v", st)
	}

	// Running InstallAll again skips already up-to-date harnesses.
	res2 := installAll(context.Background(), []Harness{cursor(), windsurf(), claudeApp()}, url)
	if len(res2.Changed) != 0 || len(res2.Failed) != 0 {
		t.Fatalf("expected no-op second installAll, got %+v", res2)
	}

	// UninstallAll removes all installed harnesses, even if a harness reports not found.
	orphanedClaudeDir := filepath.Join(base, "claude-orphan")
	if err := os.MkdirAll(orphanedClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", orphanedClaudeDir)
	if err := os.WriteFile(
		filepath.Join(orphanedClaudeDir, ".claude.json"),
		[]byte(`{"mcpServers":{"cat":{"url":"http://127.0.0.1:7788/api/mcp"},"other":{"url":"http://example.com"}}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	orphanClaude := claude()
	orphanClaude.find = func() (string, bool) { return "", false }

	uRes := uninstallAll(context.Background(), []Harness{cursor(), windsurf(), claudeApp(), orphanClaude})
	if len(uRes.Changed) != 3 || len(uRes.Failed) != 0 {
		t.Fatalf("expected 3 uninstalled with 0 failures, got %+v", uRes)
	}
	if st := StatusOf(cursor(), url); st.Installed {
		t.Fatalf("cursor still installed: %+v", st)
	}
	if st := StatusOf(windsurf(), url); st.Installed {
		t.Fatalf("windsurf still installed: %+v", st)
	}
	if st := StatusOf(orphanClaude, url); st.Installed {
		t.Fatalf("orphaned claude entry still installed: %+v", st)
	}
}
