BINARY := otel-ecom-load-genrator
CMD    := ./cmd/otel-ecom-load-genrator

.PHONY: help build test test-race e2e lint fmt validate run-local clean

help:
	@echo "build      Build binaries into ./bin"
	@echo "test       Run unit and integration tests"
	@echo "test-race  Run tests with the race detector"
	@echo "e2e        Run end-to-end tests against an in-process OTLP server"
	@echo "lint       go vet plus a gofmt check"
	@echo "fmt        Format all Go files"
	@echo "validate   Validate the shipped topologies"
	@echo "run-local  Run for 30s against a local collector"

build:
	mkdir -p bin
	go build -o bin/$(BINARY) $(CMD)
	GOOS=linux  GOARCH=amd64 go build -o bin/$(BINARY)_linux_amd64  $(CMD)
	GOOS=linux  GOARCH=arm64 go build -o bin/$(BINARY)_linux_arm64  $(CMD)
	GOOS=darwin GOARCH=arm64 go build -o bin/$(BINARY)_darwin_arm64 $(CMD)

test:
	go test ./internal/...

test-race:
	go test -race ./...

e2e:
	go test -v ./test/e2e/

lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "lint clean"

fmt:
	gofmt -w .

validate: build
	./bin/$(BINARY) --topology topologies/shop-smoke.json --validate-only
	./bin/$(BINARY) --topology topologies/shop-full.json  --validate-only

# Requires `docker compose up -d` first.
run-local: build
	./bin/$(BINARY) --topology topologies/shop-full.json --backend local --duration 30s

clean:
	rm -rf bin
