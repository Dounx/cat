#!/bin/sh
# Development loop behind `make dev`: the desktop app (or, with
# DEV_TARGET=web as `make dev-web` sets it, the headless catd) plus the Vite
# dev server.
#
#   - frontend/src is served by Vite with hot module replacement. The hub
#     proxies its UI to Vite, so the desktop window (and the hub's address in
#     a browser) hot-reloads too.
#   - Go sources, plugin manifests and the TypeScript plugins are polled once
#     a second; on a change the plugins and the hub are rebuilt and restarted
#     (the desktop window closes and reopens). A failed build leaves the
#     running hub alone.
#
# Arguments are passed on to cat-desktop or catd.
set -u
cd "$(dirname "$0")/.."

EXE=${EXE:-}
MAKE=${MAKE:-make}
UI_DEV_PORT=${UI_DEV_PORT:-5173}
DEV_TARGET=${DEV_TARGET:-desktop}
hub=
vite=
stamp=$(mktemp)

# The desktop build here is a development one: no production tag (so the
# webview has its inspector) and no frontend build, since the UI comes from
# Vite.
if [ "$DEV_TARGET" = web ]; then
	bin=catd
	build() { "$MAKE" -s plugins catd; }
else
	bin=cat-desktop-dev
	build() { "$MAKE" -s plugins && (cd desktop && go build -o "../bin/$bin$EXE" .); }
fi

start_hub() {
	"./bin/$bin$EXE" --plugins ./plugins --ui-dev "http://localhost:$UI_DEV_PORT" "$@" &
	hub=$!
}

stop_hub() {
	[ -n "$hub" ] || return 0
	kill "$hub" 2>/dev/null
	wait "$hub" 2>/dev/null
	hub=
}

cleanup() {
	trap - INT TERM EXIT
	stop_hub
	[ -n "$vite" ] && kill "$vite" 2>/dev/null
	rm -f "$stamp"
	exit 0
}
trap cleanup INT TERM EXIT

# Files whose change means a rebuild. Build outputs and dependencies are
# pruned so the build itself never triggers another one.
changed() {
	find . \
		\( -name .git -o -name node_modules -o -name bin -o -name dist -o -path ./frontend \) -prune -o \
		-type f -newer "$stamp" \
		\( -name '*.go' -o -name go.mod -o -name go.sum -o -name plugin.json \
		-o -path './plugins/*/src/*' -o -path './sdk/node/src/*' \) \
		-print | head -n 5
}

touch "$stamp"
build || exit 1
start_hub "$@"

(cd frontend && pnpm install --silent && exec pnpm dev --port "$UI_DEV_PORT" --strictPort) &
vite=$!

while sleep 1; do
	files=$(changed)
	[ -n "$files" ] || continue
	touch "$stamp"
	echo "dev: changed:" $files
	if build; then
		stop_hub
		start_hub "$@"
		echo "dev: hub restarted"
	else
		echo "dev: build failed, keeping the running hub"
	fi
done
