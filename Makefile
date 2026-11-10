# Cat event hub — build entry points.
GOOS := $(shell go env GOOS)
EXE := $(if $(filter windows,$(GOOS)),.exe,)
GO_PLUGINS := telegram onebot wecom rss webwatch apiwatch schedule agent-claude agent-codex agent-deepseek llm
NODE_PLUGINS := discord

.PHONY: all frontend plugins catd desktop app test run dev clean

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
	cd sdk/node && npx tsc -p .
	cd plugins/discord && npx tsc -p .

# Build everything, then start the hub with the plugins in this source tree.
# Extra flags go through ARGS, e.g. `make run ARGS="--addr 127.0.0.1:8080"`.
run: all
	./bin/catd$(EXE) --plugins ./plugins $(ARGS)

# Headless hub using the plugins in this source tree.
dev: plugins
	go run ./cmd/catd --plugins ./plugins

clean:
	rm -rf bin frontend/dist/assets
	rm -f plugins/*/cat-plugin-* desktop/*.syso
