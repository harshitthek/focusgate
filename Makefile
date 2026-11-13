.PHONY: all build test test-race lint fmt clean run release

BINARY_NAME=focusgate
DIST_DIR=dist

all: fmt lint test build

build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY_NAME) ./cmd/focusgate

test:
	go test -v -count=1 ./...

test-race:
	go test -v -race -count=1 ./...

lint:
	go vet ./...
	@if [ -n "$$(gofmt -s -l .)" ]; then \
		echo "Files require gofmt formatting:"; \
		gofmt -s -l .; \
		exit 1; \
	fi

fmt:
	gofmt -s -w .

run: build
	./$(BINARY_NAME) serve --config focusgate.yaml

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME).exe
	rm -rf $(DIST_DIR)

release:
	powershell -ExecutionPolicy Bypass -File scripts/build-release.ps1
