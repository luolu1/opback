BIN_DIR := bin
VERSION ?= 0.1.0
LDFLAGS := -s -w
PANEL_LDFLAGS := $(LDFLAGS) -X main.version=$(VERSION)

.PHONY: all build test vet fmt clean panel agent cross

all: build

build: panel agent

panel:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(PANEL_LDFLAGS)" -o $(BIN_DIR)/opback-panel ./cmd/panel

agent:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/opback-agent ./cmd/agent

# Build agent binaries for common platforms (handy for pushing to remote hosts).
cross:
	@mkdir -p $(BIN_DIR)
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/opback-agent-linux-amd64   ./cmd/agent
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/opback-agent-linux-arm64   ./cmd/agent
	GOOS=linux   GOARCH=arm   CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/opback-agent-linux-arm       ./cmd/agent
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/opback-agent-windows-amd64.exe ./cmd/agent
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/opback-agent-darwin-arm64  ./cmd/agent

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf $(BIN_DIR)
