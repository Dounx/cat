package main

import (
	"strings"
	"testing"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/store"
)

func TestTraySummary(t *testing.T) {
	plugins := []plugin.Manifest{
		{ID: "http", Singleton: true},
		{ID: "rss", DefaultInstance: true},
		{ID: "telegram"},
		{ID: "websocket"},
	}

	label, tip, item := traySummary(plugins, []engine.InstanceView{
		{Instance: store.Instance{ID: "http", Plugin: "http", Name: "HTTP", Enabled: true}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "rss", Plugin: "rss", Name: "RSS", Enabled: true}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "tg-off", Plugin: "telegram", Name: "Disabled Bot", Enabled: false}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "tg-ok", Plugin: "telegram", Name: "Main Bot", Enabled: true}, Status: engine.StatusRunning},
	})
	if label != "" || tip != "Cat 事件中心" || item != "" {
		t.Fatalf("healthy/ignored summary = (%q, %q, %q)", label, tip, item)
	}

	label, tip, item = traySummary(plugins, []engine.InstanceView{
		{Instance: store.Instance{ID: "tg-1", Plugin: "telegram", Name: "Ops Bot", Enabled: true}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "ws-1", Plugin: "websocket", Name: "Gateway", Enabled: true}, Status: engine.StatusError},
	})
	if label != " ⚠" {
		t.Fatalf("label = %q", label)
	}
	if !strings.Contains(tip, "2 个接入出错") || !strings.Contains(tip, "Ops Bot、Gateway") {
		t.Fatalf("tooltip = %q", tip)
	}
	if item != "⚠ 2 个接入出错（打开查看）" {
		t.Fatalf("menuText = %q", item)
	}

	_, tip4, _ := traySummary(plugins, []engine.InstanceView{
		{Instance: store.Instance{ID: "a", Plugin: "telegram", Name: "A", Enabled: true}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "b", Plugin: "telegram", Name: "B", Enabled: true}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "c", Plugin: "telegram", Name: "C", Enabled: true}, Status: engine.StatusError},
		{Instance: store.Instance{ID: "d", Plugin: "telegram", Name: "D", Enabled: true}, Status: engine.StatusError},
	})
	if !strings.Contains(tip4, "4 个接入出错：A、B、C 等") {
		t.Fatalf("tip4 = %q", tip4)
	}
}
