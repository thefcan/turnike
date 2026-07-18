BIN_DIR := bin

.PHONY: build test test-integration lint run tidy demo load deploy

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

# Runs the M4 multi-instance bypass demo end to end: builds the demo
# images, drives both arms through nginx, writes bench/demo_bypass_*.txt.
demo:
	./scripts/demo_bypass.sh

# Runs the M6 k6 latency & admission matrix (9 recorded runs) against
# the demo topology, writes raw outputs to bench/load/.
load:
	./scripts/load_test.sh

# Deploys the single-instance live demo to Fly.io. Builds with the LOCAL
# docker daemon (--local-only) so the build context never uploads to Fly's
# remote builder — the local-only agent files never leave this machine.
# Fly Machines are amd64, so the platform is pinned (built via emulation on
# Apple Silicon). Requires flyctl + `fly auth login` + an app already created
# from fly.toml — see DEPLOY.md for the one-time setup.
deploy:
	DOCKER_DEFAULT_PLATFORM=linux/amd64 flyctl deploy --local-only
