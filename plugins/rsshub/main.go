// Command cat-plugin-rsshub watches routes on an RSSHub instance and emits an
// event for every new item. The instance can be one that already runs
// somewhere, or one this plugin runs itself: in a Docker container, or from
// source with the machine's Node.js.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/feedwatch"
	"github.com/dounx/cat/plugins/internal/sidecar"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

func str(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return strings.TrimSpace(s)
	}
	if m[k] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[k]))
}

// managed reports whether this plugin runs the RSSHub instance itself.
func managed(mode string) bool { return mode == "docker" || mode == "local" }

func New() cat.Plugin {
	p := feedwatch.New(feedwatch.RSSHub)
	watch := p.Start
	var hub *sidecar.Process
	p.Start = func(ctx context.Context, h *cat.Host) error {
		if mode := str(h.Config, "mode"); managed(mode) {
			port, err := strconv.Atoi(str(h.Config, "port"))
			if err != nil {
				return errors.New("缺少端口配置，请重新运行接入向导")
			}
			key := str(h.Config, "access_key")
			if mode == "docker" {
				hub, err = runDocker(ctx, h, port, key)
			} else {
				hub, err = runLocal(ctx, h, port, key)
			}
			if err != nil {
				return err
			}
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err = awaitReady(rctx, hub, port, key, func(l string) { h.Log("debug", "%s", l) })
			cancel()
			if err != nil {
				hub.Stop()
				return err
			}
			h.Log("info", "RSSHub 已启动：%s", baseURL(port))
			go func(hub *sidecar.Process) { // keep its console from filling up
				for l := range hub.Lines {
					if l = plain(l); l != "" {
						h.Log("debug", "%s", l)
					}
				}
				if ctx.Err() == nil {
					h.SetStatus("error", "RSSHub 意外退出，请重新启用这个实例")
				}
			}(hub)
			// The routes are fetched from the instance we just started.
			h.Config["base_url"] = baseURL(port)
		}
		if err := watch(ctx, h); err != nil {
			hub.Stop()
			return err
		}
		return nil
	}
	p.Stop = func() { hub.Stop() }
	p.Setup = setup
	return p
}

func runDocker(ctx context.Context, h *cat.Host, port int, key string) (*sidecar.Process, error) {
	if err := sidecar.DockerReady(); err != nil {
		return nil, err
	}
	if !sidecar.DockerImagePresent(dockerImage) { // e.g. the pinned revision moved on
		h.Log("info", "正在拉取镜像 %s", dockerImage)
		if err := sidecar.DockerPull(ctx, dockerImage, nil); err != nil {
			return nil, err
		}
	}
	return startDocker(h.DataDir, port, key)
}

func runLocal(ctx context.Context, h *cat.Host, port int, key string) (*sidecar.Process, error) {
	dir, err := h.Dependency(ctx, "rsshub")
	if err != nil {
		return nil, err
	}
	return startLocal(dir, port, key)
}
