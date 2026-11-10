# Cat event hub — build entry points.
EXE := $(if $(filter windows,$(shell go env GOOS)),.exe,)
GO_PLUGINS := telegram onebot wecom rss webwatch apiwatch schedule agent-claude agent-codex agent-deepseek llm
NODE_PLUGINS := discord

.PHONY: all frontend plugins catd desktop app test dev clean

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

# The desktop shell is its own Go module (it needs CGO and the platform's
# webview libraries, which the headless build does not).
desktop: frontend
	cd desktop && go build -tags production -ldflags "-s -w" -o ../bin/cat-desktop$(EXE) .

# macOS: wrap the desktop binary and the plugins into Cat.app.
app: desktop plugins
	sh desktop/package-macos.sh

test:
	go test ./...
	cd sdk/go && go test ./...
	cd desktop && go vet ./...
	cd sdk/node && npx tsc -p .
	cd plugins/discord && npx tsc -p .

# Headless hub using the plugins in this source tree.
dev: plugins
	go run ./cmd/catd --plugins ./plugins

clean:
	rm -rf bin frontend/dist/assets
	rm -f plugins/*/cat-plugin-*
