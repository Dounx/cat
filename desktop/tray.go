package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
)

// traySummary computes the tray badge label, tooltip and trouble menu item
// text for the current plugin and instance state. Built-in singletons and
// default instances that require no user setup are ignored.
func traySummary(plugins []plugin.Manifest, instances []engine.InstanceView) (label, tooltip, menuText string) {
	byID := make(map[string]plugin.Manifest, len(plugins))
	for _, p := range plugins {
		byID[p.ID] = p
	}
	var names []string
	for _, inst := range instances {
		if !inst.Enabled || inst.Status != engine.StatusError {
			continue
		}
		if m, ok := byID[inst.Plugin]; ok && (m.Singleton || (m.DefaultInstance && inst.ID == m.ID)) {
			continue
		}
		name := inst.Name
		if name == "" {
			name = inst.ID
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", "Cat 事件中心", ""
	}
	shown := names
	suffix := ""
	if len(shown) > 3 {
		shown = shown[:3]
		suffix = " 等"
	}
	return " ⚠", fmt.Sprintf("Cat 事件中心 · %d 个接入出错：%s%s", len(names), strings.Join(shown, "、"), suffix),
		fmt.Sprintf("⚠ %d 个接入出错（打开查看）", len(names))
}

func openPath(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
