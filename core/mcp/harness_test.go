package mcp

import "testing"

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
