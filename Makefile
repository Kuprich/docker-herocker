BINARY=dockerherocker
BUILD_DIR=build

.PHONY: build run clean dev deps test test-e2e lint vet

build:
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY) .

run: build
	./$(BUILD_DIR)/$(BINARY)

dev:
	@go run .

deps:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
	go clean

lint:
	golangci-lint run ./...

vet:
	go vet ./...

test: build
	go test ./...

test-e2e: build
	python3 scripts/e2e.py