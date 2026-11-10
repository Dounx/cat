#!/bin/sh
# Development loop behind `make dev`: the hub plus the Vite dev server.
#
#   - frontend/src is served by Vite with hot module replacement; open the
#     URL Vite prints, not the hub's (that one serves the last `pnpm build`).
#   - Go sources, plugin manifests and the TypeScript plugins are polled once
#     a second; on a change the plugins and catd are rebuilt and the hub is
#     restarted. A failed build leaves the running hub alone.
#
# Arguments are passed on to catd.
set -u
cd "$(dirname "$0")/.."

EXE=${EXE:-}
MAKE=${MAKE:-make}
hub=
vite=
stamp=$(mktemp)

build() {
	"$MAKE" -s plugins catd
}

start_hub() {
	"./bin/catd$EXE" --plugins ./plugins "$@" &
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

(cd frontend && pnpm install --silent && exec pnpm dev) &
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
