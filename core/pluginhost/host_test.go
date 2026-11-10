package pluginhost_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/pluginhost"
	"github.com/dounx/cat/core/store"
)

// The same scenarios run against the echo plugin written with each SDK, so
// the two SDKs cannot drift apart.
var flavors = []string{"go", "node"}

func eachSDK(t *testing.T, fn func(t *testing.T, e *engine.Engine, ph *pluginhost.Host)) {
	for _, flavor := range flavors {
		t.Run(flavor, func(t *testing.T) {
			e, ph := setup(t, flavor)
			fn(t, e, ph)
		})
	}
}

// buildEcho installs the test plugin into root/echo with a plugin.json.
func buildEcho(t *testing.T, root, flavor string) {
	t.Helper()
	dir := filepath.Join(root, "echo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var command []string
	if flavor == "node" {
		if _, err := exec.LookPath("bun"); err != nil {
			t.Skip("bun not installed")
		}
		script, _ := filepath.Abs("../../sdk/node/testdata/echo.ts")
		command = []string{"bun", "run", script}
	} else {
		bin := "echo-plugin"
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, bin), "./testdata/echo")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build echo plugin: %v\n%s", err, out)
		}
		command = []string{"./" + bin}
	}
	manifest := map[string]any{
		"id": "echo", "name": "Echo", "version": "0.0.1", "protocol": 1,
		"command": command, "capabilities": []string{"source", "action"},
		"ingress": true, "setup": true,
		"actions": []map[string]any{{"name": "echo", "title": "Echo"}},
	}
	b, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, flavor string) (*engine.Engine, *pluginhost.Host) {
	t.Helper()
	root := t.TempDir()
	buildEcho(t, root, flavor)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, t.TempDir(), nil)
	ph := pluginhost.New(e, root)
	if errs := ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })
	return e, ph
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func status(e *engine.Engine, id string) string {
	v, _ := e.Instance(id)
	return v.Status
}

func eventsOfType(e *engine.Engine, typ string) int {
	list, _ := e.Store.ListEvents(store.EventFilter{Type: typ})
	return len(list)
}

func TestPluginLifecycle(t *testing.T) {
	eachSDK(t, func(t *testing.T, e *engine.Engine, _ *pluginhost.Host) {
		if m, ok := e.Plugin("echo"); !ok || m.Builtin {
			t.Fatalf("echo plugin not registered as external: %+v", m)
		}
		inst, err := e.SaveInstance(store.Instance{Plugin: "echo", Enabled: true, Config: map[string]any{"greeting": "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 10*time.Second, func() bool { return status(e, inst.ID) == engine.StatusRunning })

		// The plugin emitted an event on start, carrying its config.
		started, _ := e.Store.ListEvents(store.EventFilter{Type: "echo.started"})
		if len(started) != 1 || started[0].Source != inst.ID || started[0].Plugin != "echo" || started[0].Payload["greeting"] != "hi" {
			t.Fatalf("start event: %+v", started)
		}

		// Action round trip, including an event emitted from inside the action.
		out, err := e.Invoke(context.Background(), inst.ID, "echo", map[string]any{"text": "meow"}, plugin.InvokeContext{Event: &started[0]})
		if err != nil || out["text"] != "meow" || out["trigger"] != "echo.started" {
			t.Fatalf("echo: %v %v", out, err)
		}
		if eventsOfType(e, "echo.said") != 1 {
			t.Fatal("event emitted by action not stored")
		}

		// A failing action reports its error and partial output.
		out, err = e.Invoke(context.Background(), inst.ID, "fail", nil, plugin.InvokeContext{})
		if err == nil || err.Error() != "boom" || out["partial"] != true {
			t.Fatalf("fail: %v %v", out, err)
		}
		if _, err := e.Invoke(context.Background(), inst.ID, "nope", nil, plugin.InvokeContext{}); err == nil {
			t.Fatal("unknown action should fail")
		}

		// Cancelling the caller's context cancels the handler in the plugin.
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err = e.Invoke(ctx, inst.ID, "wait", nil, plugin.InvokeContext{})
		cancel()
		if err == nil {
			t.Fatal("wait should time out")
		}

		// Ingress round trip.
		resp, err := e.Ingress(context.Background(), inst.ID, plugin.IngressRequest{Method: "POST", Path: "/", Body: []byte("ping")})
		if err != nil || resp.Status != 200 || string(resp.Body) != "got:ping" {
			t.Fatalf("ingress: %+v %v", resp, err)
		}

		// Crash: the host notices, restarts the process, and state persisted.
		_, _ = e.Invoke(context.Background(), inst.ID, "crash", nil, plugin.InvokeContext{})
		waitFor(t, "error status after crash", 5*time.Second, func() bool { return status(e, inst.ID) == engine.StatusError })
		waitFor(t, "restart", 10*time.Second, func() bool { return status(e, inst.ID) == engine.StatusRunning })
		started, _ = e.Store.ListEvents(store.EventFilter{Type: "echo.started"})
		if len(started) != 2 || started[0].Payload["starts"] != float64(2) {
			t.Fatalf("after restart: %+v", started)
		}
		if out, err := e.Invoke(context.Background(), inst.ID, "echo", map[string]any{"text": "again"}, plugin.InvokeContext{}); err != nil || out["text"] != "again" {
			t.Fatalf("echo after restart: %v %v", out, err)
		}
	})
}

func TestStartFailureIsReported(t *testing.T) {
	eachSDK(t, func(t *testing.T, e *engine.Engine, _ *pluginhost.Host) {
		inst, _ := e.SaveInstance(store.Instance{Plugin: "echo", Enabled: true, Config: map[string]any{"fail": "yes"}})
		waitFor(t, "error", 10*time.Second, func() bool { return status(e, inst.ID) == engine.StatusError })
		if v, _ := e.Instance(inst.ID); v.Error != "configured to fail" {
			t.Fatalf("error message: %q", v.Error)
		}
	})
}

func TestSetupWizard(t *testing.T) {
	eachSDK(t, func(t *testing.T, e *engine.Engine, ph *pluginhost.Host) {
		view, err := ph.StartSetup("echo", "", "My Echo")
		if err != nil {
			t.Fatal(err)
		}
		state := func() pluginhost.SetupView { v, _ := ph.Setup(view.ID); return v }
		waitFor(t, "form", 10*time.Second, func() bool { return state().State == pluginhost.SetupWaiting })
		if s := state().Step; s["kind"] != "form" || s["title"] != "Greeting" {
			t.Fatalf("step: %+v", s)
		}
		if err := ph.AnswerSetup(view.ID, map[string]any{"greeting": "hello"}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "confirm", 5*time.Second, func() bool { s := state(); return s.State == pluginhost.SetupWaiting && s.Step["kind"] == "confirm" })
		if err := ph.AnswerSetup(view.ID, map[string]any{"confirmed": true}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "done", 10*time.Second, func() bool { return state().State == pluginhost.SetupDone })
		inst, ok := e.Instance(view.InstanceID)
		if !ok || inst.Name != "My Echo" || inst.Config["greeting"] != "hello" {
			t.Fatalf("instance: %+v", inst)
		}
		waitFor(t, "instance running", 10*time.Second, func() bool { return status(e, view.InstanceID) == engine.StatusRunning })
	})
}

func TestSetupCancel(t *testing.T) {
	eachSDK(t, func(t *testing.T, e *engine.Engine, ph *pluginhost.Host) {
		view, _ := ph.StartSetup("echo", "", "")
		waitFor(t, "form", 10*time.Second, func() bool { v, _ := ph.Setup(view.ID); return v.State == pluginhost.SetupWaiting })
		ph.CancelSetup(view.ID)
		waitFor(t, "cancelled", 5*time.Second, func() bool { v, _ := ph.Setup(view.ID); return v.State == pluginhost.SetupCancelled })
		if _, ok := e.Instance(view.InstanceID); ok {
			t.Fatal("cancelled setup must not create an instance")
		}
	})
}
