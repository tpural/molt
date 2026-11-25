VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test lint fmt backend e2e

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/molt ./cmd/molt

test:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	gofumpt -w .

# The backend image: Paper with molt-agent.
backend:
	docker build -f backend/Dockerfile --build-arg VERSION=$(VERSION) -t molt-paper:dev .

# A real swap with a bot playing through it (needs docker and node).
e2e: backend
	cd e2e/bot && npm ci --silent
	cd e2e && go test -count=1 -tags e2e -v -timeout 20m .
