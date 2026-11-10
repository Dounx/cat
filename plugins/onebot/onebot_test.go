package main_test

import (
	"archive/zip"
	"bytes"
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

// TestManagedNapCatEndToEnd runs the whole one-click flow under the real
// plugin host: consent, download, checksum, unpack, configure, launch, QR
// code, login detection, then the instance starting with quick login and
// exchanging messages. NapCat itself is replaced by testdata/fakenapcat.
func TestManagedNapCatEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	tmp := t.TempDir()

	// The "release": a zip holding a launcher and the fake NapCat binary.
	fake := filepath.Join(tmp, "fakenapcat")
	build(t, fake, "./testdata/fakenapcat")
	bin, _ := os.ReadFile(fake)
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	add := func(name string, body []byte) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, _ := zw.CreateHeader(h)
		w.Write(body)
	}
	add("launcher.sh", []byte("#!/bin/sh\nexec \"$(dirname \"$0\")/fakenapcat\" \"$@\"\n"))
	add("fakenapcat", bin)
	zw.Close()
	sum := sha256.Sum256(zbuf.Bytes())
	downloads := 0
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		w.Write(zbuf.Bytes())
	}))
	defer release.Close()

	// Install the plugin with a manifest whose dependency points at that zip.
	root := filepath.Join(tmp, "plugins")
	pdir := filepath.Join(root, "onebot")
	_ = os.MkdirAll(pdir, 0o755)
	build(t, filepath.Join(pdir, "cat-plugin-onebot"), ".")
	raw, _ := os.ReadFile("plugin.json")
	var manifest map[string]any
	_ = json.Unmarshal(raw, &manifest)
	manifest["dependencies"] = []map[string]any{{
		"name": "napcat", "title": "NapCat", "version": "0.0.1-test",
		"platforms": map[string]any{"any": map[string]any{"url": release.URL + "/NapCat.Shell.zip", "sha256": hex.EncodeToString(sum[:]), "format": "zip"}},
	}}
	b, _ := json.Marshal(manifest)
	_ = os.WriteFile(filepath.Join(pdir, "plugin.json"), b, 0o644)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, filepath.Join(tmp, "data"), nil)
	ph := pluginhost.New(e, root)
	if errs := ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })

	view, err := ph.StartSetup("onebot", "", "我的 QQ")
	if err != nil {
		t.Fatal(err)
	}
	state := func() pluginhost.SetupView { v, _ := ph.Setup(view.ID); return v }
	waiting := func(kind string) func() bool {
		return func() bool {
			s := state()
			if s.State == pluginhost.SetupError {
				t.Fatalf("setup failed: %s", s.Error)
			}
			return s.State == pluginhost.SetupWaiting && s.Step["kind"] == kind
		}
	}
	answer := func(v map[string]any) {
		t.Helper()
		if err := ph.AnswerSetup(view.ID, v); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "mode form", waiting("form"))
	answer(map[string]any{"mode": "managed"})

	// The host asks before downloading, naming the source; nothing is fetched until the user agrees.
	waitFor(t, "download consent", waiting("confirm"))
	if desc, _ := state().Step["description"].(string); !strings.Contains(desc, release.URL) || downloads != 0 {
		t.Fatalf("consent step should name the source before any download: downloads=%d desc=%q", downloads, desc)
	}
	answer(map[string]any{"confirmed": true})

	// The QR code NapCat printed reaches the wizard.
	waitFor(t, "QR code", func() bool {
		s := state()
		if s.State == pluginhost.SetupError {
			t.Fatalf("setup failed: %s", s.Error)
		}
		return s.Step["kind"] == "qrcode" || s.State == pluginhost.SetupDone
	})
	if s := state(); s.State != pluginhost.SetupDone && s.Step["text"] != "https://txz.qq.com/p?k=FAKEQR&f=1" {
		t.Fatalf("qr step: %+v", s.Step)
	}
	waitFor(t, "setup done", func() bool {
		s := state()
		if s.State == pluginhost.SetupError {
			t.Fatalf("setup failed: %s", s.Error)
		}
		return s.State == pluginhost.SetupDone
	})
	if downloads != 1 {
		t.Fatalf("downloads = %d", downloads)
	}

	// The saved instance starts NapCat again, this time with quick login.
	inst, ok := e.Instance(view.InstanceID)
	if !ok || inst.Name != "我的 QQ" || inst.Config["mode"] != "managed" || inst.Config["uin"] != "10001" {
		t.Fatalf("instance: %+v", inst)
	}
	if inst.Config["token"] != engine.SecretMask {
		t.Fatalf("the generated token should be stored as a secret: %v", inst.Config["token"])
	}
	waitFor(t, "instance running", func() bool {
		v, _ := e.Instance(view.InstanceID)
		if v.Status == engine.StatusError {
			t.Fatalf("instance failed: %s", v.Error)
		}
		return v.Status == engine.StatusRunning
	})
	napcatDir := filepath.Join(tmp, "data", "instances", view.InstanceID, "napcat")
	fakeLog := func() string { b, _ := os.ReadFile(filepath.Join(napcatDir, "fake.log")); return string(b) }
	if !strings.Contains(fakeLog(), "quick-login 10001") {
		t.Fatalf("instance should start NapCat with quick login; log:\n%s", fakeLog())
	}
	var cfg map[string]any
	cb, _ := os.ReadFile(filepath.Join(napcatDir, "config", "webui.json"))
	_ = json.Unmarshal(cb, &cfg)
	if cfg["host"] != "127.0.0.1" {
		t.Fatalf("NapCat's WebUI must be confined to loopback: %v", cfg)
	}

	// A group message that mentions the bot arrives as an event...
	waitFor(t, "message event", func() bool {
		l, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
		return len(l) >= 1
	})
	list, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
	ev := list[0]
	if ev.Conversation.ID != "group:30003" || ev.Conversation.Kind != "group" || ev.Actor.ID != "20002" || ev.Actor.Name != "群名片" {
		t.Fatalf("event: %+v %+v", ev.Conversation, ev.Actor)
	}
	if ev.Payload["text"] != "你好" || ev.Payload["mentioned"] != true || ev.Payload["message_id"] != "777" {
		t.Fatalf("payload: %+v", ev.Payload)
	}
	// ...and a reply goes back to the same group, quoting it.
	out, err := e.Invoke(context.Background(), view.InstanceID, "reply", map[string]any{"text": "喵"}, plugin.InvokeContext{Event: &ev})
	if err != nil || out["message_id"] != "888" {
		t.Fatalf("reply: %v %v", out, err)
	}
	if log := fakeLog(); !strings.Contains(log, `"group_id":30003`) || !strings.Contains(log, `"type":"reply"`) || !strings.Contains(log, "喵") {
		t.Fatalf("send_msg not as expected:\n%s", log)
	}
	if _, err := e.Invoke(context.Background(), view.InstanceID, "send", map[string]any{"conversation_id": "nonsense", "text": "x"}, plugin.InvokeContext{}); err == nil {
		t.Fatal("a malformed conversation id should be rejected")
	}

	// Stopping the instance takes NapCat down with it.
	if err := e.DeleteInstance(view.InstanceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "NapCat to exit", func() bool {
		out, _ := exec.Command("pgrep", "-f", napcatDir).Output()
		return len(bytes.TrimSpace(out)) == 0
	})
}

// On a platform the release does not cover, the wizard explains and falls
// back to connecting to a service the user runs.
func TestUnsupportedPlatformFallsBackToExternal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shipped manifest supports Windows")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "plugins")
	pdir := filepath.Join(root, "onebot")
	_ = os.MkdirAll(pdir, 0o755)
	build(t, filepath.Join(pdir, "cat-plugin-onebot"), ".")
	raw, _ := os.ReadFile("plugin.json")
	_ = os.WriteFile(filepath.Join(pdir, "plugin.json"), raw, 0o644) // the real manifest

	st, _ := store.Open(":memory:")
	e := engine.New(st, filepath.Join(tmp, "data"), nil)
	ph := pluginhost.New(e, root)
	if errs := ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	_ = e.Start()
	t.Cleanup(func() { e.Stop(); st.Close() })

	view, err := ph.StartSetup("onebot", "", "")
	if err != nil {
		t.Fatal(err)
	}
	state := func() pluginhost.SetupView { v, _ := ph.Setup(view.ID); return v }
	waitFor(t, "mode form", func() bool { return state().State == pluginhost.SetupWaiting })
	_ = ph.AnswerSetup(view.ID, map[string]any{"mode": "managed"})
	waitFor(t, "explanation", func() bool {
		s := state()
		return s.State == pluginhost.SetupWaiting && s.Step["kind"] == "confirm" && s.Step["title"] == "无法自动配置 NapCat"
	})
	if desc, _ := state().Step["description"].(string); !strings.Contains(desc, "不支持当前系统") {
		t.Fatalf("explanation: %q", desc)
	}
	_ = ph.AnswerSetup(view.ID, map[string]any{"confirmed": true})
	waitFor(t, "external form", func() bool {
		s := state()
		return s.State == pluginhost.SetupWaiting && s.Step["title"] == "连接已有的 OneBot 服务"
	})
	// An address nothing listens on is reported, and the form comes back.
	_ = ph.AnswerSetup(view.ID, map[string]any{"ws_url": "ws://127.0.0.1:1"})
	waitFor(t, "connection problem shown", func() bool {
		desc, _ := state().Step["description"].(string)
		return state().State == pluginhost.SetupWaiting && strings.Contains(desc, "无法连接")
	})
	ph.CancelSetup(view.ID)
}

// TestDockerNapCatEndToEnd runs the Docker flow under the real plugin host:
// consent before pulling the image, configure, run the container, QR code,
// login detection, then the instance starting with quick login. The docker
// CLI is replaced by testdata/fakedocker, which runs testdata/fakenapcat.
func TestDockerNapCatEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	_ = os.MkdirAll(bin, 0o755)
	build(t, filepath.Join(bin, "fakenapcat"), "./testdata/fakenapcat")
	script, _ := os.ReadFile("testdata/fakedocker/docker")
	_ = os.WriteFile(filepath.Join(bin, "docker"), script, 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")) // plugins inherit it
	dockerLog := func() string { b, _ := os.ReadFile(filepath.Join(bin, "docker.log")); return string(b) }

	root := filepath.Join(tmp, "plugins")
	pdir := filepath.Join(root, "onebot")
	_ = os.MkdirAll(pdir, 0o755)
	build(t, filepath.Join(pdir, "cat-plugin-onebot"), ".")
	raw, _ := os.ReadFile("plugin.json")
	_ = os.WriteFile(filepath.Join(pdir, "plugin.json"), raw, 0o644)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, filepath.Join(tmp, "data"), nil)
	ph := pluginhost.New(e, root)
	if errs := ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })

	view, err := ph.StartSetup("onebot", "", "容器里的 QQ")
	if err != nil {
		t.Fatal(err)
	}
	state := func() pluginhost.SetupView {
		v, _ := ph.Setup(view.ID)
		if v.State == pluginhost.SetupError {
			t.Fatalf("setup failed: %s", v.Error)
		}
		return v
	}
	waitFor(t, "mode form", func() bool { s := state(); return s.State == pluginhost.SetupWaiting && s.Step["kind"] == "form" })
	// With docker on PATH, the Docker way is the one suggested.
	if v, _ := state().Step["values"].(map[string]any); v["mode"] != "docker" {
		t.Fatalf("suggested mode: %+v", state().Step)
	}
	_ = ph.AnswerSetup(view.ID, map[string]any{"mode": "docker"})

	// Nothing is pulled until the user agrees, and the step names the image.
	waitFor(t, "pull consent", func() bool { s := state(); return s.State == pluginhost.SetupWaiting && s.Step["kind"] == "confirm" })
	if desc, _ := state().Step["description"].(string); !strings.Contains(desc, "mlikiowa/napcat-docker:") || strings.Contains(dockerLog(), "pull") {
		t.Fatalf("consent step should name the image before any pull: desc=%q log:\n%s", desc, dockerLog())
	}
	_ = ph.AnswerSetup(view.ID, map[string]any{"confirmed": true})

	waitFor(t, "QR code", func() bool { s := state(); return s.Step["kind"] == "qrcode" || s.State == pluginhost.SetupDone })
	if s := state(); s.State != pluginhost.SetupDone && s.Step["text"] != "https://txz.qq.com/p?k=FAKEQR&f=1" {
		t.Fatalf("qr step: %+v", s.Step)
	}
	waitFor(t, "setup done", func() bool { return state().State == pluginhost.SetupDone })

	inst, ok := e.Instance(view.InstanceID)
	if !ok || inst.Config["mode"] != "docker" || inst.Config["uin"] != "10001" || inst.Config["token"] != engine.SecretMask {
		t.Fatalf("instance: %+v", inst)
	}
	waitFor(t, "instance running", func() bool {
		v, _ := e.Instance(view.InstanceID)
		if v.Status == engine.StatusError {
			t.Fatalf("instance failed: %s", v.Error)
		}
		return v.Status == engine.StatusRunning
	})
	dataDir := filepath.Join(tmp, "data", "instances", view.InstanceID, "napcat-docker")
	fakeLog, _ := os.ReadFile(filepath.Join(dataDir, "fake.log"))
	// Inside the container NapCat listens on every interface at its usual port...
	if !strings.Contains(string(fakeLog), "quick-login 10001") || !strings.Contains(string(fakeLog), "listen 0.0.0.0:3001") {
		t.Fatalf("container should start with quick login on 0.0.0.0:3001; log:\n%s", fakeLog)
	}
	// ...but only the OneBot port is published, and only on loopback; the
	// image was pulled once, and login state is kept outside the container.
	log := dockerLog()
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "run ") && (strings.Count(line, " -p ") != 1 || !strings.Contains(line, " -p 127.0.0.1:") ||
			!strings.Contains(line, filepath.Join(dataDir, "qq")+":/app/.config/QQ")) {
			t.Fatalf("docker run: %s", line)
		}
	}
	if strings.Count(log, "pull ") != 1 || strings.Count(log, "\nrun ") != 2 {
		t.Fatalf("expected one pull and two runs:\n%s", log)
	}

	waitFor(t, "message event", func() bool {
		l, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
		return len(l) >= 1
	})

	// Stopping the instance removes the container.
	if err := e.DeleteInstance(view.InstanceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "container to be removed", func() bool {
		pids, _ := filepath.Glob(filepath.Join(bin, "*.pid"))
		out, _ := exec.Command("pgrep", "-f", filepath.Join(bin, "fakenapcat")).Output()
		return len(pids) == 0 && len(bytes.TrimSpace(out)) == 0
	})
}
