.PHONY: all test lint build clean

# Default target
all: lint test build

# Run tests with race detector
test:
	go test -v -race ./...

# Run golangci-lint
lint:
	golangci-lint run

# Build the project (if applicable, adjust path as needed)
build:
	go build ./...

# Clean build artifacts
clean:
	go clean
	rm -f raft
