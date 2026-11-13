package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// runProfile is the DeepSeek Harness profile that answers one task and exits.
// The other profiles boot an app (desktop, web, ACP), so they cannot be run
// here; what they can lend is their model choice.
const runProfile = "headless"

// dshHome is where DeepSeek Harness keeps its profiles.
func dshHome() string {
	if d := os.Getenv("DSH_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dsh")
}

type dshProfile struct {
	name, provider, model string
}

var (
	entryID   = regexp.MustCompile(`^- id:\s*["']?([^\s"'#]+)`)
	fieldLine = regexp.MustCompile(`(?m)^\s+(provider|model):\s*(.+)$`)
)

// modelEntries returns the entries of a profile's patch list that decide which
// model answers: the default model and every llm-* provider setup. The file is
// a top-level YAML list, so entries are cut where a line starts with "- ";
// they are passed on verbatim, which keeps tags such as !!js intact.
func modelEntries(profile string) (entries []string, chosen dshProfile) {
	chosen.name = profile
	raw, err := os.ReadFile(filepath.Join(dshHome(), "profiles", profile, "cordis.patch.yml"))
	if err != nil {
		return nil, chosen
	}
	var blocks []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			blocks = append(blocks, line)
		case len(blocks) > 0 && !strings.HasPrefix(line, "#"):
			blocks[len(blocks)-1] += "\n" + line
		}
	}
	for _, b := range blocks {
		m := entryID.FindStringSubmatch(b)
		if m == nil {
			continue
		}
		if m[1] == "agent-default-model" {
			for _, f := range fieldLine.FindAllStringSubmatch(b, -1) {
				v := strings.Trim(strings.TrimSpace(strings.SplitN(f[2], " #", 2)[0]), `"'`)
				if f[1] == "provider" {
					chosen.provider = v
				} else {
					chosen.model = v
				}
			}
		} else if !strings.HasPrefix(m[1], "llm-") {
			continue
		}
		entries = append(entries, strings.TrimRight(b, "\n"))
	}
	return entries, chosen
}

// A model a profile's provider setup declares.
type dshModel struct {
	provider, id, name string
}

// declaredModels reads the models out of llm-* entries: under "providers:",
// each key is a provider and each "- id:" below it one of its models.
func declaredModels(entries []string) []dshModel {
	clean := func(v string) string {
		return strings.Trim(strings.TrimSpace(strings.SplitN(v, " #", 2)[0]), `"'`)
	}
	var out []dshModel
	for _, e := range entries {
		if m := entryID.FindStringSubmatch(e); m == nil || !strings.HasPrefix(m[1], "llm-") {
			continue
		}
		at, provider := -1, "" // indent of "providers:", and whose models follow
		for _, line := range strings.Split(e, "\n") {
			text := strings.TrimSpace(line)
			if text == "" || strings.HasPrefix(text, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " "))
			switch {
			case text == "providers:":
				at, provider = indent, ""
			case at < 0:
			case indent <= at:
				at = -1
			case indent == at+2 && strings.HasSuffix(text, ":"):
				provider = clean(strings.TrimSuffix(text, ":"))
			case provider != "" && strings.HasPrefix(text, "- id:"):
				out = append(out, dshModel{provider: provider, id: clean(strings.TrimPrefix(text, "- id:"))})
			case len(out) > 0 && out[len(out)-1].name == "" && strings.HasPrefix(text, "name:"):
				out[len(out)-1].name = clean(strings.TrimPrefix(text, "name:"))
			}
		}
	}
	return out
}

// listProfiles returns the profiles that have picked a model, plus the run
// profile itself.
func listProfiles() []dshProfile {
	dirs, _ := os.ReadDir(filepath.Join(dshHome(), "profiles"))
	out := []dshProfile{}
	for _, d := range dirs {
		if !d.IsDir() || d.Name() == runProfile {
			continue
		}
		if _, p := modelEntries(d.Name()); p.model != "" {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	_, own := modelEntries(runProfile)
	return append(out, own)
}

// overlay writes a patch file for `dsh --patch`: the model setup borrowed from
// another profile and, when model is given ("provider/model"), that model in
// place of the profile's own choice. It is rebuilt on every run, so a model
// switched in that profile takes effect here too. Empty when there is nothing
// to change.
func overlay(profile, model, dataDir string) (string, error) {
	var entries []string
	if profile != "" && profile != runProfile {
		var p dshProfile
		if entries, p = modelEntries(profile); p.model == "" {
			return "", errors.New("DeepSeek Harness 的档案「" + profile + "」里没有选定模型。请在这个接入的配置里换一个档案")
		}
	}
	if model != "" {
		provider, name, ok := strings.Cut(model, "/")
		if !ok || provider == "" || name == "" {
			return "", errors.New("模型「" + model + "」要写成 提供方/模型，例如 magpie/auto/glm-5.3 或 deepseek-official/deepseek-flash")
		}
		kept := entries[:0:0]
		for _, e := range entries {
			if m := entryID.FindStringSubmatch(e); m == nil || m[1] != "agent-default-model" {
				kept = append(kept, e)
			}
		}
		entries = append(kept, fmt.Sprintf("- id: agent-default-model\n  config:\n    provider: %q\n    model: %q", provider, name))
	}
	if len(entries) == 0 {
		return "", nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	body := "# Written by Cat: which model DeepSeek Harness answers with.\n" + strings.Join(entries, "\n") + "\n"
	// Named by content: runs with different models may overlap.
	sum := sha1.Sum([]byte(body))
	path := filepath.Join(dataDir, fmt.Sprintf("harness-model-%x.patch.yml", sum[:4]))
	return path, os.WriteFile(path, []byte(body), 0o600)
}
