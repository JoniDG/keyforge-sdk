.PHONY: help test cover lint clean

COVERAGE_MIN := 95

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

test: ## Run unit tests with coverage
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@echo "---"
	@go tool cover -func=coverage.out | tail -1

cover: test ## Generate HTML coverage report
	go tool cover -html=coverage.out -o coverage.html
	@echo "Open coverage.html"

lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { echo "ERROR: golangci-lint not found. Run: brew install golangci-lint"; exit 1; }
	golangci-lint run ./...

clean: ## Remove coverage files
	rm -f coverage.out coverage.html
