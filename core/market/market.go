// Package market installs plugins from an index: a JSON file listing plugins
// with, per platform, a download URL and its SHA-256.
package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/pluginhost"
)

// Entry is one plugin in the index.
type Entry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Category is the plugin's category; empty means "channel".
	Category string `json:"category,omitempty"`
	Author   string `json:"author,omitempty"`
	Homepage string `json:"homepage,omitempty"`
	// Platforms maps "os/arch", "os" or "any" to the archive for it. The
	// archive must contain plugin.json at its root.
	Platforms map[string]pluginhost.Artifact `json:"platforms"`
}

// Index is the market's index file.
type Index struct {
	Plugins []Entry `json:"plugins"`
}

// Listing is an index entry plus what is known locally.
type Listing struct {
	Entry
	Supported        bool   `json:"supported"`         // has an artifact for this platform
	InstalledVersion string `json:"installed_version"` // "" when not installed
	Bundled          bool   `json:"bundled"`           // provided by the app itself, not the market
}

type Market struct {
	// URL of the index; empty means no market is configured.
	URL string
	// Dir is where plugins are installed (<data>/plugins).
	Dir     string
	Host    *pluginhost.Host
	Engine  *engine.Engine
	Client  *http.Client
	staging *pluginhost.Deps
}

func New(url, dir string, host *pluginhost.Host, eng *engine.Engine) *Market {
	return &Market{URL: url, Dir: dir, Host: host, Engine: eng,
		staging: &pluginhost.Deps{Dir: filepath.Join(eng.DataDir(), "market-staging")}}
}

var ErrNotConfigured = errors.New("还没有配置插件市场地址（启动参数 --market 或环境变量 CAT_MARKET）")

func (m *Market) fetch(ctx context.Context) (*Index, error) {
	if m.URL == "" {
		return nil, ErrNotConfigured
	}
	var raw []byte
	if path, ok := strings.CutPrefix(m.URL, "file://"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		raw = b
	} else {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", m.URL, nil)
		if err != nil {
			return nil, err
		}
		client := m.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("无法读取插件市场：%w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("无法读取插件市场：HTTP %d", resp.StatusCode)
		}
		if raw, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20)); err != nil {
			return nil, err
		}
	}
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("插件市场索引格式错误：%w", err)
	}
	return &idx, nil
}

func (m *Market) installed() map[string]*pluginhost.Manifest {
	out := map[string]*pluginhost.Manifest{}
	for _, man := range m.Host.Manifests() {
		out[man.ID] = man
	}
	return out
}

func (m *Market) inMarketDir(man *pluginhost.Manifest) bool {
	root, _ := filepath.Abs(m.Dir)
	return filepath.Dir(man.Dir) == root
}

// List returns the index annotated with local state.
func (m *Market) List(ctx context.Context) ([]Listing, error) {
	idx, err := m.fetch(ctx)
	if err != nil {
		return nil, err
	}
	have := m.installed()
	out := make([]Listing, 0, len(idx.Plugins))
	for _, e := range idx.Plugins {
		l := Listing{Entry: e}
		l.Category, _ = plugin.NormalizeCategory(e.Category)
		_, perr := pluginhost.Dependency{Platforms: e.Platforms}.Pick()
		l.Supported = perr == nil
		if man, ok := have[e.ID]; ok {
			l.InstalledVersion = man.Version
			l.Bundled = !m.inMarketDir(man)
		}
		out = append(out, l)
	}
	return out, nil
}

// Install downloads, verifies and installs (or updates) a plugin, then
// restarts its instances. progress may be nil.
func (m *Market) Install(ctx context.Context, pluginID string, progress func(done, total int64)) error {
	idx, err := m.fetch(ctx)
	if err != nil {
		return err
	}
	var entry *Entry
	for i := range idx.Plugins {
		if idx.Plugins[i].ID == pluginID {
			entry = &idx.Plugins[i]
		}
	}
	if entry == nil {
		return fmt.Errorf("插件市场里没有 %q", pluginID)
	}
	if strings.ContainsAny(entry.ID, `/\. `) || entry.ID == "" {
		return fmt.Errorf("非法的插件标识 %q", entry.ID)
	}
	dep := pluginhost.Dependency{Name: entry.ID, Version: entry.Version, Platforms: entry.Platforms}
	staged, err := m.staging.Install(ctx, "plugin", dep, progress)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(staged))
	man, err := pluginhost.LoadManifest(staged)
	if err != nil {
		return fmt.Errorf("下载的插件无效：%w", err)
	}
	if man.ID != entry.ID {
		return fmt.Errorf("下载的插件标识是 %q，与市场条目 %q 不符，已拒绝安装", man.ID, entry.ID)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	// Stop the plugin's instances before replacing its files (a running
	// executable can not be overwritten on Windows).
	instances := m.instancesOf(entry.ID)
	for _, inst := range instances {
		stopped := inst
		stopped.Enabled = false
		_, _ = m.Engine.SaveInstance(stopped.Instance)
	}
	dest := filepath.Join(m.Dir, entry.ID)
	old := dest + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, dest); err != nil {
		_ = os.Rename(old, dest)
		return err
	}
	_ = os.RemoveAll(old)
	_ = os.Remove(filepath.Join(dest, ".installed"))
	m.Host.Scan()
	for _, inst := range instances {
		_, _ = m.Engine.SaveInstance(inst.Instance)
	}
	m.Engine.Publish("plugins", map[string]any{"changed": true})
	return nil
}

func (m *Market) instancesOf(pluginID string) []engine.InstanceView {
	var out []engine.InstanceView
	for _, inst := range m.Engine.Instances() {
		if inst.Plugin == pluginID {
			out = append(out, inst)
		}
	}
	return out
}

// Uninstall removes a plugin installed from the market. Its instances must
// be deleted first.
func (m *Market) Uninstall(pluginID string) error {
	man, ok := m.installed()[pluginID]
	if !ok {
		return fmt.Errorf("插件 %q 未安装", pluginID)
	}
	if !m.inMarketDir(man) {
		return errors.New("这个插件是随应用提供的，不能卸载")
	}
	if n := len(m.instancesOf(pluginID)); n > 0 {
		return fmt.Errorf("还有 %d 个接入在使用这个插件，请先删除它们", n)
	}
	if err := os.RemoveAll(man.Dir); err != nil {
		return err
	}
	m.Host.Scan()
	m.Engine.Publish("plugins", map[string]any{"changed": true})
	return nil
}
