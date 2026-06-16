.PHONY: help build server cli run test lint fmt tidy clean docker docker-dev

BIN_DIR := ./bin
SERVER_BIN := $(BIN_DIR)/paperboy-server
CLI_BIN := $(BIN_DIR)/paperboy

help: ## show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: server cli ## build server and CLI binaries

server: ## build the HTTP server
	mkdir -p $(BIN_DIR)
	go build -o $(SERVER_BIN) ./cmd/paperboy-server

cli: ## build the debug CLI
	mkdir -p $(BIN_DIR)
	go build -o $(CLI_BIN) ./cmd/paperboy

run: server ## run the server with default config
	$(SERVER_BIN)

test: ## run all tests
	go test -race -count=1 ./...

lint: ## run golangci-lint
	golangci-lint run ./...

fmt: ## format all Go files
	gofmt -s -w .

tidy: ## tidy go.mod / go.sum
	go mod tidy

clean: ## remove build artifacts
	rm -rf $(BIN_DIR)

docker: ## build production docker image
	docker build -f docker/Dockerfile -t paperboy:latest .

docker-dev: ## run the dev container (compose)
	docker compose -f compose.dev.yaml up --build
