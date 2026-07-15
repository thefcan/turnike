BIN_DIR := bin

.PHONY: build test lint run tidy

build:
	go build -o $(BIN_DIR)/gateway ./cmd/gateway
	go build -o $(BIN_DIR)/mock ./mock

test:
	go test -race ./...

lint:
	golangci-lint run

run: build
	./$(BIN_DIR)/gateway -config config.example.yaml

tidy:
	go mod tidy
