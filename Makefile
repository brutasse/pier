CONFIG ?= configs/example.yaml

.PHONY: test test-integration build run fmt vet

test:
	go test ./...

test-integration:
	go test -tags integration ./test/integration

build:
	go build -trimpath -o bin/pier ./cmd/pier

run: build
	./bin/pier -config $(CONFIG)

fmt:
	gofmt -w .

vet:
	go vet ./...
