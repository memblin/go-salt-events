# salt-events task runner. Run `just` or `just --list`.

default: check

deps:
    go mod download

fmt:
    go tool golangci-lint fmt

fmt-check:
    go tool golangci-lint fmt --diff

vet:
    go vet ./...

lint:
    go tool golangci-lint run

test:
    go test -count=1 ./...

test-race:
    go test -count=1 -race ./...

# Race + coverage, used by CI. Excludes the integration tag on purpose.
test-ci:
    go test -count=1 -race -coverprofile=coverage.out ./...

# Requires a live salt-master and root. Auto-skips when the socket is absent or
# unreadable; set SALT_EVENTS_REQUIRE_BUS=1 to turn those skips into failures.
# Never set that in CI — no runner can host a master, so it would be red always.
test-integration:
    go test -count=1 -p 1 -tags=integration ./...

vuln:
    go tool govulncheck ./...

# Fails if `go mod tidy` would change go.mod / go.sum.
tidy:
    go mod tidy
    git diff --exit-code -- go.mod go.sum

check: fmt-check vet lint test

build:
    go build -o salt-events ./cmd/salt-events

# Record real frames off the live bus into testdata (spec §13). Needs sudo.
capture n="200":
    sudo go run ./cmd/salt-events --capture={{n}} --capture-out=internal/saltipc/testdata/live-frames.bin
