BIN_DIR := bin

.PHONY: build test test-integration lint run tidy

build:
	go build -o $(BIN_DIR)/gateway ./cmd/gateway
	go build -o $(BIN_DIR)/mock ./mock

test:
	go test -race ./...

# Runs the whole suite including the redis integration tests against the
# dev-compose redis. Deliberately no auto-down: the dev redis may be in
# shared use.
test-integration:
	docker compose -f docker-compose.dev.yml up -d --wait redis
	REDIS_ADDR=localhost:6379 go test -race ./...

lint:
	golangci-lint run

run: build
	./$(BIN_DIR)/gateway -config config.example.yaml

tidy:
	go mod tidy
