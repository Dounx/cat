# syntax=docker/dockerfile:1

# Stage 1: Build the embedded web UI
FROM node:22-slim AS frontend-builder
WORKDIR /src/frontend
RUN corepack enable
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY frontend/ ./
RUN pnpm build

# Stage 2: Build catd and bundled Go plugins
FROM golang:1.27-bookworm AS go-builder
WORKDIR /src
ENV CGO_ENABLED=0
ARG CAT_VERSION=dev

COPY go.mod go.sum ./
COPY sdk/go/go.mod sdk/go/go.sum ./sdk/go/
RUN go mod download

COPY . .
COPY --from=frontend-builder /src/frontend/dist ./frontend/dist

RUN LDFLAGS="-s -w"; \
    if [ "${CAT_VERSION}" != "dev" ] && [ -n "${CAT_VERSION}" ]; then \
      LDFLAGS="${LDFLAGS} -X github.com/dounx/cat/app.Version=${CAT_VERSION}"; \
    fi; \
    go build -ldflags "${LDFLAGS}" -o /out/catd ./cmd/catd

RUN mkdir -p /out/plugins && \
    for p in telegram feishu dingtalk email github onebot qqbot wecom rss rsshub webwatch apiwatch schedule claude-desktop chatgpt-desktop deepseek-harness llm magpie jev; do \
      if [ -d "plugins/${p}" ]; then \
        mkdir -p "/out/plugins/${p}" && \
        cp "plugins/${p}/plugin.json" "/out/plugins/${p}/plugin.json" && \
        go build -ldflags "-s -w" -o "/out/plugins/${p}/cat-plugin-${p}" "./plugins/${p}" || exit 1; \
      fi; \
    done

# Stage 3: Minimal non-root runtime image
FROM debian:bookworm-slim AS runtime
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata curl && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd -r cat && \
    useradd -r -g cat -d /data -s /sbin/nologin cat && \
    mkdir -p /data /app && \
    chown -R cat:cat /data /app

WORKDIR /app
COPY --from=go-builder --chown=cat:cat /out/catd /app/catd
COPY --from=go-builder --chown=cat:cat /out/plugins /app/plugins

USER cat
VOLUME ["/data"]

ENV CAT_DATA=/data \
    CAT_ADDR=0.0.0.0:7788 \
    CAT_UPDATE=off

EXPOSE 7788

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:7788/api/status >/dev/null || exit 1

ENTRYPOINT ["/app/catd"]
