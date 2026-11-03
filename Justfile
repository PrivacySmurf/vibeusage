set dotenv-load := true
set unstable := true

# List all available commands
[private]
default:
    @just --list --list-submodules

# Build the binary
build *ARGS:
    go build {{ ARGS }} -o vibeusage ./cmd/vibeusage

# Run tests with coverage
coverage *ARGS:
    go test ./... -race -cover {{ ARGS }}

# Format code
fmt *ARGS='.':
    gofmt -w {{ ARGS }}

# Run all pre-commit hooks
lint:
    pre-commit run --all-files

# Check GitHub Action pin versions against live upstream releases
check-actions:
    pre-commit run --hook-stage manual check --all-files

# CLI
run *ARGS:
    go run ./cmd/vibeusage {{ ARGS }}

# Regenerate README screenshots (requires freeze)
screenshots:
    bash scripts/screenshots.sh

# Run tests
test *ARGS:
    go test ./... -race {{ ARGS }}

# Run static analysis (host OS plus Linux/Windows cross-OS build-tag checks)
vet:
    go vet ./...
    GOOS=linux GOARCH=amd64 go vet ./...
    GOOS=windows GOARCH=amd64 go vet ./...

# Check dependencies for known vulnerabilities
vuln:
    go tool govulncheck ./...

# Run all checks
check: test lint vet vuln tidy-check

# Tidy go.mod/go.sum
tidy:
    go mod tidy

# Verify go.mod/go.sum are tidy without rewriting them
tidy-check:
    go mod tidy -diff
