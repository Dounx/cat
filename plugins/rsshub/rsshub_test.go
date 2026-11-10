package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/pluginhost"
	"github.com/dounx/cat/core/store"
)

func build(t *testing.T, out, pkg string) {
	t.Helper()
	if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// world is a real plugin host with the RSSHub plugin installed, and a PATH
// on which docker, node and pnpm are the stand-ins from testdata/fakebin.
type world struct {
	t   *testing.T
	bin string // where the stand-ins live, and write their logs
	tmp string
	e   *engine.Engine
	ph  *pluginhost.Host
}

// newWorld sets that up. tools are the stand-ins to put on PATH; manifest may
// adjust plugin.json before the host reads it.
func newWorld(t *testing.T, tools []string, manifest func(m map[string]any)) *world {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-ins are shell scripts")
	}
	tmp := t.TempDir()
	w := &world{t: t, tmp: tmp, bin: filepath.Join(tmp, "bin")}
	_ = os.MkdirAll(w.bin, 0o755)
	build(t, filepath.Join(w.bin, "fakersshub"), "./testdata/fakersshub")
	for _, name := range tools {
		script, err := os.ReadFile(filepath.Join("testdata", "fakebin", name))
		if err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(w.bin, name), script, 0o755)
	}
	_ = os.WriteFile(filepath.Join(w.bin, "node-version"), []byte("v24.15.0\n"), 0o644)

	root := filepath.Join(tmp, "plugins")
	pdir := filepath.Join(root, "rsshub")
	_ = os.MkdirAll(pdir, 0o755)
	build(t, filepath.Join(pdir, "cat-plugin-rsshub"), ".")
	raw, _ := os.ReadFile("plugin.json")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if manifest != nil {
		manifest(m)
	}
	b, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(pdir, "plugin.json"), b, 0o644)

	// Plugins inherit PATH. Only the stand-ins and the basic tools their
	// scripts use are on it, so a real docker or pnpm is never reached - not
	// even one in /usr/bin, where CI runners keep docker.
	sys := filepath.Join(tmp, "sys")
	_ = os.MkdirAll(sys, 0o755)
	for _, name := range []string{"dirname", "cat", "rm", "cut", "touch", "mkdir", "pgrep"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(sys, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", w.bin+string(os.PathListSeparator)+sys)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	w.e = engine.New(st, filepath.Join(tmp, "data"), nil)
	w.ph = pluginhost.New(w.e, root)
	if errs := w.ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := w.e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.e.Stop(); st.Close() })
	return w
}

func (w *world) log(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.bin, name))
	return string(b)
}

// wizard is one run of the setup wizard.
type wizard struct {
	*world
	view pluginhost.SetupView
}

func (w *world) setup(instanceID string) *wizard {
	w.t.Helper()
	view, err := w.ph.StartSetup("rsshub", instanceID, "我的 RSSHub")
	if err != nil {
		w.t.Fatal(err)
	}
	return &wizard{world: w, view: view}
}

func (z *wizard) state() pluginhost.SetupView {
	z.t.Helper()
	v, _ := z.ph.Setup(z.view.ID)
	if v.State == pluginhost.SetupError {
		z.t.Fatalf("setup failed: %s", v.Error)
	}
	return v
}

// expect waits for the wizard to ask a step of that kind and title.
func (z *wizard) expect(kind, title string) map[string]any {
	z.t.Helper()
	waitFor(z.t, kind+" step "+title, func() bool {
		s := z.state()
		return s.State == pluginhost.SetupWaiting && s.Step["kind"] == kind && strings.Contains(s.Step["title"].(string), title)
	})
	return z.state().Step
}

func (z *wizard) answer(v map[string]any) {
	z.t.Helper()
	if err := z.ph.AnswerSetup(z.view.ID, v); err != nil {
		z.t.Fatal(err)
	}
}

// done waits for the wizard to finish and the instance to run.
func (z *wizard) done() engine.InstanceView {
	z.t.Helper()
	waitFor(z.t, "setup done", func() bool { return z.state().State == pluginhost.SetupDone })
	waitFor(z.t, "instance running", func() bool {
		v, _ := z.e.Instance(z.view.InstanceID)
		if v.Status == engine.StatusError {
			z.t.Fatalf("instance failed: %s", v.Error)
		}
		return v.Status == engine.StatusRunning
	})
	inst, _ := z.e.Instance(z.view.InstanceID)
	return inst
}

// checkFeed has the instance fetch its route from the RSSHub it runs.
func (w *world) checkFeed(id string) {
	w.t.Helper()
	out, err := w.e.Invoke(context.Background(), id, "check", map[string]any{}, plugin.InvokeContext{})
	if err != nil || out["checked"] != float64(1) || out["failed"] != float64(0) {
		w.t.Fatalf("check: %v %v", out, err)
	}
}

// TestDockerEndToEnd runs the Docker way under the real plugin host: consent
// before the image is pulled, a trial start, then the instance running RSSHub
// in a container and reading a route from it.
func TestDockerEndToEnd(t *testing.T) {
	w := newWorld(t, []string{"docker"}, nil)
	z := w.setup("")

	// With docker on PATH, the Docker way is the one suggested.
	if v, _ := z.expect("form", "用哪个 RSSHub 实例")["values"].(map[string]any); v["mode"] != "docker" {
		t.Fatalf("suggested mode: %+v", v)
	}
	z.answer(map[string]any{"mode": "docker"})

	// Nothing is pulled until the user agrees, and the step names the image.
	step := z.expect("confirm", "需要拉取 RSSHub 镜像")
	if desc, _ := step["description"].(string); !strings.Contains(desc, dockerImage) || strings.Contains(w.log("docker.log"), "pull") {
		t.Fatalf("consent step should name the image before any pull: desc=%q log:\n%s", desc, w.log("docker.log"))
	}
	z.answer(map[string]any{"confirmed": true})

	// Once RSSHub has been seen to start, only the routes are left to ask.
	step = z.expect("form", "订阅哪些路由")
	if props := step["schema"].(map[string]any)["properties"].(map[string]any); props["base_url"] != nil {
		t.Fatalf("a managed instance has no address to fill in: %+v", props)
	}
	z.answer(map[string]any{"routes": "/test/feed"})

	inst := z.done()
	if inst.Config["mode"] != "docker" || inst.Config["access_key"] != engine.SecretMask || inst.Config["base_url"] != nil {
		t.Fatalf("instance: %+v", inst.Config)
	}
	w.checkFeed(inst.ID)

	// The port is published on loopback only, the access key stays off the
	// command line, and the image was pulled once.
	log := w.log("docker.log")
	runs := 0
	for _, line := range strings.Split(log, "\n") {
		if !strings.HasPrefix(line, "run ") {
			continue
		}
		runs++
		if strings.Count(line, " -p ") != 1 || !strings.Contains(line, " -p 127.0.0.1:") || !strings.Contains(line, ":1200 ") ||
			!strings.Contains(line, " -e ACCESS_KEY ") || !strings.HasSuffix(line, " "+dockerImage) {
			t.Fatalf("docker run: %s", line)
		}
	}
	if strings.Count(log, "pull ") != 1 || runs != 2 {
		t.Fatalf("expected one pull and two runs (trial, instance):\n%s", log)
	}
	if hub := w.log("hub.log"); strings.Contains(hub, "key=false") {
		t.Fatalf("RSSHub must always run with an access key:\n%s", hub)
	}

	// Stopping the instance removes the container.
	if err := w.e.DeleteInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "container to be removed", func() bool {
		pids, _ := filepath.Glob(filepath.Join(w.bin, "*.pid"))
		out, _ := exec.Command("pgrep", "-f", filepath.Join(w.bin, "fakersshub")).Output()
		return len(pids) == 0 && len(bytes.TrimSpace(out)) == 0
	})
}

// sourceRelease serves a stand-in for RSSHub's source archive and returns the
// manifest dependency pointing at it.
func sourceRelease(t *testing.T, downloads *int) map[string]any {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(`{"name":"rsshub","engines":{"node":"^24.15.0 || ^26.13.0"},"packageManager":"pnpm@12.18.0"}`)
	_ = tw.WriteHeader(&tar.Header{Name: "RSSHub-abc/", Mode: 0o755, Typeflag: tar.TypeDir})
	_ = tw.WriteHeader(&tar.Header{Name: "RSSHub-abc/package.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*downloads++
		w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return map[string]any{
		"name": "rsshub", "title": "RSSHub 源码", "version": "0.0.1-test",
		"platforms": map[string]any{"any": map[string]any{"url": srv.URL + "/src.tar.gz", "sha256": hex.EncodeToString(sum[:]), "format": "tar.gz", "strip": 1}},
	}
}

// TestLocalEndToEnd runs the local way: consent, download, install and build
// with pnpm, a trial start, then the instance running RSSHub with Node.js.
func TestLocalEndToEnd(t *testing.T) {
	downloads := 0
	w := newWorld(t, []string{"node", "pnpm"}, func(m map[string]any) {
		m["dependencies"] = []map[string]any{sourceRelease(t, &downloads)}
	})
	z := w.setup("")

	// Without docker, an existing instance is what is suggested.
	if v, _ := z.expect("form", "用哪个 RSSHub 实例")["values"].(map[string]any); v["mode"] != "external" {
		t.Fatalf("suggested mode: %+v", v)
	}
	z.answer(map[string]any{"mode": "local"})

	// The host asks before downloading; nothing is fetched or built until then.
	z.expect("confirm", "下载 RSSHub 源码")
	if downloads != 0 || w.log("pnpm.log") != "" {
		t.Fatalf("nothing should happen before consent: downloads=%d pnpm:\n%s", downloads, w.log("pnpm.log"))
	}
	z.answer(map[string]any{"confirmed": true})

	z.expect("form", "订阅哪些路由")
	z.answer(map[string]any{"routes": "/test/feed"})
	inst := z.done()
	if inst.Config["mode"] != "local" || inst.Config["access_key"] != engine.SecretMask {
		t.Fatalf("instance: %+v", inst.Config)
	}
	w.checkFeed(inst.ID)

	// Installed and built once, without downloading a browser...
	if got := w.log("pnpm.log"); got != "install --frozen-lockfile (skip-browser=1)\nbuild (skip-browser=1)\n" {
		t.Fatalf("pnpm was run as:\n%s", got)
	}
	// ...and run twice (trial, instance), in production mode, on loopback only.
	if got := w.log("hub.log"); strings.Count(got, "\n") != 2 || strings.Count(got, "listen 127.0.0.1:") != 2 ||
		strings.Contains(got, "key=false") || strings.Count(got, "node_env=production") != 2 {
		t.Fatalf("RSSHub was started as:\n%s", got)
	}

	// Re-running the wizard keeps the port and the routes, and builds nothing.
	port := inst.Config["port"]
	z = w.setup(inst.ID)
	z.expect("form", "用哪个 RSSHub 实例")
	z.answer(map[string]any{"mode": "local"})
	if v, _ := z.expect("form", "订阅哪些路由")["values"].(map[string]any); v["routes"] != "/test/feed" {
		t.Fatalf("routes should be kept: %+v", v)
	}
	z.answer(map[string]any{"routes": "/test/feed"})
	if inst = z.done(); inst.Config["port"] != port || downloads != 1 || strings.Count(w.log("pnpm.log"), "\n") != 2 {
		t.Fatalf("re-run: port %v -> %v, downloads=%d, pnpm:\n%s", port, inst.Config["port"], downloads, w.log("pnpm.log"))
	}
	w.checkFeed(inst.ID)

	// Stopping the instance takes RSSHub down with it.
	if err := w.e.DeleteInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "RSSHub to exit", func() bool {
		out, _ := exec.Command("pgrep", "-f", filepath.Join(w.bin, "fakersshub")).Output()
		return len(bytes.TrimSpace(out)) == 0
	})
}

// What stands in the way of running RSSHub here is explained, and the wizard
// falls back to an instance that runs elsewhere.
func TestFallsBackToExternal(t *testing.T) {
	downloads := 0
	w := newWorld(t, []string{"node", "pnpm"}, func(m map[string]any) {
		m["dependencies"] = []map[string]any{sourceRelease(t, &downloads)}
	})

	for _, c := range []struct {
		mode, why string
		arrange   func()
	}{
		{"docker", "没有找到 docker 命令", func() {}},
		{"local", "需要 Node.js ^24.15.0 || ^26.13.0，本机是 v22.1.0", func() {
			_ = os.WriteFile(filepath.Join(w.bin, "node-version"), []byte("v22.1.0\n"), 0o644)
		}},
		{"local", "ERR_PNPM build exploded", func() {
			_ = os.WriteFile(filepath.Join(w.bin, "node-version"), []byte("v26.13.1\n"), 0o644)
			_ = os.WriteFile(filepath.Join(w.bin, "build-fails"), nil, 0o644)
		}},
	} {
		c.arrange()
		z := w.setup("")
		z.expect("form", "用哪个 RSSHub 实例")
		z.answer(map[string]any{"mode": c.mode})
		if c.mode == "local" && downloads == 0 {
			z.expect("confirm", "下载 RSSHub 源码")
			z.answer(map[string]any{"confirmed": true})
		}
		step := z.expect("confirm", "无法自动运行 RSSHub")
		if desc, _ := step["description"].(string); !strings.Contains(desc, c.why) {
			t.Fatalf("%s: explanation %q should mention %q", c.mode, desc, c.why)
		}
		z.answer(map[string]any{"confirmed": true})
		step = z.expect("form", "订阅哪些路由")
		if v, _ := step["values"].(map[string]any); v["base_url"] != publicHub {
			t.Fatalf("%s: the external form should offer the public instance: %+v", c.mode, v)
		}
		w.ph.CancelSetup(z.view.ID)
		waitFor(t, "wizard to close", func() bool { v, _ := w.ph.Setup(z.view.ID); return v.State == pluginhost.SetupCancelled })
	}
}

func TestNodeSatisfies(t *testing.T) {
	const want = "^24.15.0 || ^26.13.0"
	for version, ok := range map[string]bool{
		"v24.15.0": true, "v24.16.2": true, "v26.13.0": true, "v26.20.1": true,
		"v24.14.9": false, "v25.9.0": false, "v26.12.0": false, "v22.11.0": false, "v27.0.0": false,
	} {
		if nodeSatisfies(version, want) != ok {
			t.Errorf("nodeSatisfies(%q, %q) should be %v", version, want, ok)
		}
	}
	if !nodeSatisfies("v22.0.0", ">=22") || nodeSatisfies("v20.1.0", ">=22.0.0") || !nodeSatisfies("v20.1.0", "lts/*") || !nodeSatisfies("v20.1.0", "") {
		t.Error(">= ranges are compared; ranges that cannot be read pass")
	}
}

// The Docker image and the source dependency must be the same RSSHub.
func TestManifestPinsTheSameRevision(t *testing.T) {
	raw, _ := os.ReadFile("plugin.json")
	var m struct {
		Dependencies []pluginhost.Dependency `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Dependencies) != 1 {
		t.Fatalf("dependencies: %v %+v", err, m.Dependencies)
	}
	dep := m.Dependencies[0]
	if art := dep.Platforms["any"]; !strings.Contains(art.URL, "/"+commit+".") || !strings.HasPrefix(commit, dep.Version) || art.Strip != 1 {
		t.Fatalf("plugin.json should pin %s: %+v", commit, dep)
	}
}
