package mcp

import (
	"context"
	"encoding/json"
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
