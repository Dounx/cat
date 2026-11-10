module github.com/dounx/cat/desktop

go 1.27.3

require (
	github.com/dounx/cat v0.0.0
	github.com/wailsapp/wails/v3 v3.1.0
	github.com/zalando/go-keyring v0.2.8
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dounx/cat/sdk/go v0.0.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/expr-lang/expr v1.17.8 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/term v0.46.0 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.62.0 // indirect
)

replace github.com/dounx/cat => ../

replace github.com/dounx/cat/sdk/go => ../sdk/go
