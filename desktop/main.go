// Command cat-desktop is the desktop shell: the same engine and web UI as
// catd, in a native window with a tray icon so it keeps running when the
// window is closed.
package main

import (
	"context"
	_ "embed"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/dounx/cat/app"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/desktop/autostart"
)

//go:embed assets/tray-template.png
var trayTemplate []byte

//go:embed assets/tray.png
var trayIcon []byte

//go:embed assets/appicon.png
var appIcon []byte

func main() {
	addr := flag.String("addr", envOr("CAT_ADDR", "127.0.0.1:7788"), "listen address")
	data := flag.String("data", os.Getenv("CAT_DATA"), "data directory (default ~/.cat)")
	plugins := flag.String("plugins", os.Getenv("CAT_PLUGINS"), "extra plugin directories")
	marketURL := flag.String("market", os.Getenv("CAT_MARKET"), "plugin market index URL")
	hidden := flag.Bool("hidden", false, "start in the tray without opening the window")
	flag.Parse()

	var window *application.WebviewWindow
	show := func() {
		if window != nil {
			window.Show()
			window.Focus()
		}
	}

	wails := application.New(application.Options{
		Name: "Cat",
		Icon: appIcon,
		Mac:  application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               "com.dounx.cat",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { show() },
		},
	})

	var secrets engine.Secrets
	if k := (keychain{service: "Cat Event Hub"}); k.usable() {
		secrets = k
	}
	hub, err := app.New(app.Options{
		Addr: *addr, DataDir: *data, PluginDirs: filepath.SplitList(*plugins),
		Secrets: secrets, Desktop: true, MarketURL: *marketURL,
		OpenURL: func(url string) error { return wails.Browser.OpenURL(url) },
	})
	if err != nil {
		// Through the logger, so it reaches the log file when the failure
		// came after that was opened.
		slog.Error("cat cannot start", "err", err)
		os.Exit(1)
	}
	// Said only now: app.New is what opens the log file.
	if secrets == nil {
		slog.Warn("no system credential store; secrets are kept in a private file instead")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := hub.Run(ctx); err != nil {
			slog.Error("hub stopped", "err", err)
		}
	}()

	window = wails.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "Cat", Width: 1180, Height: 760, MinWidth: 860, MinHeight: 560,
		URL: hub.URL(), Hidden: *hidden,
	})
	// Closing the window only hides it: the hub keeps receiving events.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		window.Hide()
		e.Cancel()
	})

	tray := wails.SystemTray.New()
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(trayTemplate)
	} else {
		tray.SetIcon(trayIcon)
	}
	tray.SetTooltip("Cat 事件中心")
	menu := wails.NewMenu()
	menu.Add("打开 Cat").OnClick(func(*application.Context) { show() })
	menu.Add("在浏览器中打开").OnClick(func(*application.Context) { _ = wails.Browser.OpenURL(hub.URL()) })
	menu.AddSeparator()
	menu.AddCheckbox("登录时启动", autostart.Enabled()).OnClick(func(c *application.Context) {
		if err := autostart.Set(c.ClickedMenuItem().Checked()); err != nil {
			slog.Error("autostart", "err", err)
		}
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) { wails.Quit() })
	tray.SetMenu(menu)

	err = wails.Run()
	cancel()
	<-done
	if err != nil {
		slog.Error("cat stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
