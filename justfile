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

# Requires a live salt-master and root. Auto-skips when the socket is absent.
test-integration:
    go test -count=1 -p 1 -tags=integration ./...

vuln:
    go tool govulncheck ./...

check: fmt-check vet lint test

build:
    go build -o salt-events ./cmd/salt-events

# Record real frames off the live bus into testdata (spec §13). Needs sudo.
capture n="200":
    sudo go run ./cmd/salt-events --capture={{n}} --capture-out=internal/saltipc/testdata/live-frames.bin
