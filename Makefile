VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION) -X github.com/Maffewem/Orchestrator/internal/agent.Version=$(VERSION)
GO_BUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

# Agent targets: every Linux machine you're likely to manage, from Pi Zero to x86 servers.
AGENT_TARGETS := amd64:amd64: arm64:arm64: armv7:arm:7 armv6:arm:6

.PHONY: all web server agent agents test dev-server dev-web clean

all: web server agents

web:
	cd web && npm ci && npm run build

server:
	$(GO_BUILD) -o bin/orchestrator-server ./cmd/orchestrator-server

agent:
	$(GO_BUILD) -o bin/orchestrator-agent ./cmd/orchestrator-agent

# Cross-compiled agents, named the way /install.sh downloads them.
agents:
	@mkdir -p bin/downloads
	@for t in $(AGENT_TARGETS); do \
		name=$${t%%:*}; rest=$${t#*:}; arch=$${rest%%:*}; arm=$${rest#*:}; \
		echo "building agent for linux/$$name"; \
		GOOS=linux GOARCH=$$arch GOARM=$$arm $(GO_BUILD) -o bin/downloads/orchestrator-agent-linux-$$name ./cmd/orchestrator-agent || exit 1; \
	done

test:
	go vet ./...
	go test ./...
	cd web && npm run typecheck

# Local development: run the API on :8080 and the Vite dev server (proxying /api) on :5173.
dev-server:
	go run ./cmd/orchestrator-server -db dev.db -downloads bin/downloads

dev-web:
	cd web && npm run dev

clean:
	rm -rf bin web/dist/assets web/dist/index.html
