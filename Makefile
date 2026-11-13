# Cat event hub — build entry points.
GOOS := $(shell go env GOOS)
EXE := $(if $(filter windows,$(GOOS)),.exe,)
GO_PLUGINS := telegram github onebot qqbot wecom rss rsshub webwatch apiwatch schedule claude-desktop chatgpt-desktop deepseek-harness llm magpie jev
NODE_PLUGINS := discord

.PHONY: all frontend plugins catd desktop app test fmt fmt-check run dev clean

all: frontend plugins catd

frontend:
	cd frontend && pnpm install && pnpm build

# Each plugin is built into its own directory, next to its plugin.json, so
# `--plugins ./plugins` runs them straight from the source tree.
plugins:
	@for p in $(GO_PLUGINS); do \
		if [ -d plugins/$$p ]; then echo "build plugin $$p"; go build -o plugins/$$p/cat-plugin-$$p$(EXE) ./plugins/$$p || exit 1; fi; \
	done
	@for p in $(NODE_PLUGINS); do \
		if [ -d plugins/$$p ]; then echo "build plugin $$p"; (cd plugins/$$p && bun install --silent && bun run build) || exit 1; fi; \
	done

catd:
	go build -o bin/catd$(EXE) ./cmd/catd

# The desktop shell is its own Go module (it needs the platform's webview
# libraries, and CGO everywhere but Windows, which the headless build does
# not). On Windows the icon and manifest are linked in from a generated .syso
# and the executable is built without a console window.
desktop: frontend
	$(if $(filter windows,$(GOOS)),sh desktop/windows/syso.sh)
	cd desktop && go build -tags production -ldflags "-s -w$(if $(filter windows,$(GOOS)), -H windowsgui)" -o ../bin/cat-desktop$(EXE) .

# Wrap the desktop binary and the plugins into the platform's package:
# bin/Cat.app on macOS, bin/Cat-linux (and a .deb) on Linux, bin/Cat-windows
# on Windows.
app: desktop plugins
	sh desktop/package-$(patsubst darwin,macos,$(GOOS)).sh

test:
	go test ./...
	cd sdk/go && go test ./...
	cd desktop && go vet ./...
	cd frontend && pnpm test
	cd sdk/node && npx tsc -p .
	cd plugins/discord && npx tsc -p .

# Go: gofmt works per file, so one pass over the tracked files covers every Go
# module in the repository (the hub, sdk/go and desktop).
# TypeScript and Vue: Prettier is a dev dependency of the frontend and reads
# .prettierrc.json at the repository root, so it needs `pnpm install` there.
PRETTIER_PATHS := src vite.config.ts ../sdk/node/src ../plugins/discord/src

fmt:
	gofmt -w $$(git ls-files '*.go')
	cd frontend && pnpm exec prettier --write $(PRETTIER_PATHS)

# What CI runs: lists the files a formatter would change, and fails if there
# are any.
fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "Not gofmt-formatted (run 'make fmt'):"; echo "$$out"; exit 1; fi
	cd frontend && pnpm exec prettier --check $(PRETTIER_PATHS)

# Build everything, then start the hub with the plugins in this source tree,
# serving the UI that was just built. Nothing is rebuilt while it runs; use
# `make dev` for that. Extra flags go through ARGS, e.g.
# `make run ARGS="--addr 127.0.0.1:8080"`.
run: all
	./bin/catd$(EXE) --plugins ./plugins $(ARGS)

# Development loop: the hub, rebuilt and restarted when Go or plugin sources
# change, plus the Vite dev server with hot reload for the UI. The hub proxies
# its UI to Vite, so http://127.0.0.1:7788 hot-reloads as well. ARGS is passed
# to catd as in `run`.
dev:
	@mkdir -p frontend/dist
	@EXE=$(EXE) MAKE=$(MAKE) sh scripts/dev.sh $(ARGS)

clean:
	rm -rf bin frontend/dist/assets
	rm -f plugins/*/cat-plugin-* desktop/*.syso
