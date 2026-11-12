package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/sidecar"
	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/jsonrpc"
)

const publicHub = "https://rsshub.app"

func randomKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cancelled(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.Is(err, cat.ErrCancelled) || (errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeCancelled)
}

func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	was := str(cfg, "mode")
	if was == "" && len(s.Existing) > 0 {
		was = "external" // an instance from before there was a choice
	}
	mode := was
	if mode == "" {
		mode = "external"
		if _, err := exec.LookPath("docker"); err == nil {
			mode = "docker"
		}
	}
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "用哪个 RSSHub 实例",
		Description: "用已有的 RSSHub：你自建的，或公共的 rsshub.app（经常被限流）。\n由 Cat 在 Docker 里运行：需要已安装 Docker。\n由 Cat 用本机的 Node.js 运行：Cat 下载 RSSHub 源码并构建，需要已安装 Node.js。\n\n由 Cat 运行的 RSSHub 只在本机可以访问，随这个接入启动和停止。",
		Values:      map[string]any{"mode": mode},
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"mode": map[string]any{"type": "string", "title": "方式", "enum": []string{"external", "docker", "local"}, "x-enum-titles": []string{"用已有的 RSSHub", "由 Cat 在 Docker 里运行", "由 Cat 用本机的 Node.js 运行"}},
		}},
	})
	if err != nil {
		return nil, err
	}
	if mode = str(out, "mode"); managed(mode) {
		prepare := prepareDocker
		if mode == "local" {
			prepare = prepareLocal
		}
		port, key, err := launch(ctx, s, cfg, prepare)
		if err == nil {
			cfg["mode"], cfg["port"], cfg["access_key"] = mode, port, key
			delete(cfg, "base_url")
			return askRoutes(ctx, s, cfg, false)
		}
		if cancelled(err) || ctx.Err() != nil {
			return nil, err
		}
		// Running it here is not possible: say why, then fall back to an
		// instance that runs elsewhere.
		ok, aerr := s.Ask(ctx, cat.Step{
			Kind: cat.StepConfirm, Title: "无法自动运行 RSSHub", URL: "https://docs.rsshub.app/deploy/",
			Description: err.Error() + "\n\n你可以按 RSSHub 文档自行部署，或者使用别的实例。点确认，填写实例地址。",
		})
		if aerr != nil {
			return nil, aerr
		}
		if c, _ := ok["confirmed"].(bool); !c {
			return nil, cat.ErrCancelled
		}
	}
	if was != "external" { // the key of an instance we ran is of no use elsewhere
		delete(cfg, "base_url")
		delete(cfg, "access_key")
	}
	cfg["mode"] = "external"
	delete(cfg, "port")
	return askRoutes(ctx, s, cfg, true)
}

// starter runs RSSHub on a port with an access key.
type starter func(port int, key string) (*sidecar.Process, error)

// prepareDocker makes sure the image is there, asking before it is pulled.
func prepareDocker(ctx context.Context, s *cat.Setup) (starter, error) {
	if err := sidecar.DockerUp(ctx, func() { s.Progress("正在启动 Docker…", -1) }); err != nil {
		return nil, err
	}
	if !sidecar.DockerImagePresent(dockerImage) {
		ok, err := s.Ask(ctx, cat.Step{
			Kind: cat.StepConfirm, Title: "需要拉取 RSSHub 镜像", URL: "https://hub.docker.com/r/diygod/rsshub",
			Description: "将从 Docker Hub 拉取 RSSHub 的官方镜像 " + dockerImage + "（解压后约 700 MB）。\n\n这个镜像不带浏览器，少数需要浏览器渲染的路由用不了。",
		})
		if err != nil {
			return nil, err
		}
		if c, _ := ok["confirmed"].(bool); !c {
			return nil, cat.ErrCancelled
		}
		s.Progress("正在拉取镜像…", -1)
		if err := sidecar.DockerPull(ctx, dockerImage, func(l string) { s.Progress("正在拉取镜像："+l, -1) }); err != nil {
			return nil, err
		}
	}
	return func(port int, key string) (*sidecar.Process, error) { return startDocker(s.DataDir, port, key) }, nil
}

// prepareLocal downloads RSSHub's source (the host asks first) and builds it.
func prepareLocal(ctx context.Context, s *cat.Setup) (starter, error) {
	if _, err := exec.LookPath("node"); err != nil {
		return nil, errors.New("没有找到 node 命令。请先安装 Node.js（nodejs.org）")
	}
	dir, err := s.Dependency(ctx, "rsshub")
	if err != nil {
		return nil, err
	}
	var last time.Time
	err = buildLocal(ctx, dir, func(stage, line string) {
		if line != "" && time.Since(last) < 200*time.Millisecond {
			return // pnpm prints faster than anyone reads
		}
		last = time.Now()
		s.Show(cat.Step{Kind: cat.StepProgress, Title: stage + "…", Description: line, Progress: -1})
	})
	if err != nil {
		return nil, err
	}
	return func(port int, key string) (*sidecar.Process, error) { return startLocal(dir, port, key) }, nil
}

// launch prepares RSSHub, starts it once to see that it works, and returns
// the port and access key the instance will run it with.
func launch(ctx context.Context, s *cat.Setup, cfg map[string]any, prepare func(context.Context, *cat.Setup) (starter, error)) (int, string, error) {
	start, err := prepare(ctx, s)
	if err != nil {
		return 0, "", err
	}
	// Keep the port across re-runs; RSSHub's usual one when it is free.
	port, _ := strconv.Atoi(str(cfg, "port"))
	if port <= 0 || portFree(port) != nil {
		if port = containerPort; portFree(port) != nil {
			if port, err = sidecar.FreePort(); err != nil {
				return 0, "", err
			}
		}
	}
	key := randomKey()
	s.Progress("正在启动 RSSHub…", -1)
	p, err := start(port, key)
	if err != nil {
		return 0, "", err
	}
	defer p.Stop()
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := awaitReady(rctx, p, port, key, nil); err != nil {
		return 0, "", err
	}
	return port, key, nil
}

// askRoutes is the last step: what to subscribe to, and for an external
// instance, where it is.
func askRoutes(ctx context.Context, s *cat.Setup, cfg map[string]any, external bool) (map[string]any, error) {
	desc := "RSSHub 已经可以运行，之后会随这个接入启动和停止。\n\n每行填一个路由，以 / 开头，如 /github/issue/DIYgod/RSSHub。有哪些路由见 RSSHub 文档。"
	order := []string{"routes"}
	props := map[string]any{
		"routes": map[string]any{"type": "string", "title": "路由", "format": "textarea"},
	}
	values := map[string]any{"routes": str(cfg, "routes")}
	if external {
		desc = "填写实例地址和要订阅的路由。每行一个路由，以 / 开头，如 /github/issue/DIYgod/RSSHub。有哪些路由见 RSSHub 文档。"
		order = []string{"base_url", "access_key", "routes"}
		props["base_url"] = map[string]any{"type": "string", "title": "实例地址", "description": "公共实例 " + publicHub + " 经常被限流，建议填自建实例。"}
		props["access_key"] = map[string]any{"type": "string", "title": "访问密钥（可选）", "format": "password", "description": "自建实例设置了 ACCESS_KEY 时填写。"}
		if values["base_url"] = str(cfg, "base_url"); values["base_url"] == "" {
			values["base_url"] = publicHub
		}
		if str(cfg, "access_key") != "" {
			props["access_key"].(map[string]any)["description"] = "已保存；留空表示不修改。"
		}
	}
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "订阅哪些路由", Description: desc, URL: "https://docs.rsshub.app/routes/",
		Values: values,
		Schema: map[string]any{"type": "object", "required": []string{"routes"}, "x-order": order, "properties": props},
	})
	if err != nil {
		return nil, err
	}
	cfg["routes"] = str(out, "routes")
	if external {
		cfg["base_url"] = strings.TrimRight(str(out, "base_url"), "/")
		if key := str(out, "access_key"); key != "" {
			cfg["access_key"] = key
		}
	}
	return cfg, nil
}
