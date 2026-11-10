# Cat event hub — build entry points.
EXE := $(if $(filter windows,$(shell go env GOOS)),.exe,)
GO_PLUGINS := telegram onebot agent-claude agent-codex agent-deepseek llm wecom
NODE_PLUGINS := discord

.PHONY: all frontend plugins catd desktop test dev clean

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

desktop: frontend
	cd cmd/cat-desktop && wails3 task build

test:
	go test ./...
	cd sdk/go && go test ./...
	cd sdk/node && npx tsc -p .
	cd plugins/discord && npx tsc -p .

# Headless hub using the plugins in this source tree.
dev: plugins
	go run ./cmd/catd --plugins ./plugins

clean:
	rm -rf bin frontend/dist/assets
	rm -f plugins/*/cat-plugin-*
