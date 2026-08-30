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

# Version stamped into the binary. Defaults to the closest tag plus the commit,
# so a local build reports something truthful rather than "dev"; the release
# workflow overrides it with the tag being built.
VERSION := env_var_or_default("VERSION", `git describe --tags --always --dirty 2>/dev/null || echo dev`)
COMMIT := `git rev-parse --short HEAD 2>/dev/null || echo unknown`
LDFLAGS := "-X main.version=" + VERSION + " -X main.commit=" + COMMIT

build:
    go build -ldflags "{{LDFLAGS}}" -o salt-events ./cmd/salt-events

# A release binary: static (no glibc coupling when a package built on one host
# is installed on another), stripped, and with local paths trimmed out.
# SOURCE_DATE_EPOCH keeps the stamped date reproducible for a given commit.
build-release goos="linux" goarch="amd64":
    #!/usr/bin/env bash
    set -euo pipefail
    date=$(date -u -d "@${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}" +%Y-%m-%dT%H:%M:%SZ)
    mkdir -p dist
    CGO_ENABLED=0 GOOS={{goos}} GOARCH={{goarch}} go build \
        -trimpath \
        -ldflags "-s -w {{LDFLAGS}} -X main.buildDate=${date}" \
        -o dist/salt-events-{{goos}}-{{goarch}}/salt-events \
        ./cmd/salt-events

# Architectures a release publishes.
#
# amd64 ONLY, and not because cross-compiling is hard: `GOARCH=arm64` with
# CGO_ENABLED=0 builds cleanly on an amd64 host and produced a valid aarch64
# binary. It is excluded because every runner here is amd64, so an arm64
# artefact would be published having never been EXECUTED on the architecture it
# targets — not tested, not smoke-run, not even started. That is a guess wearing
# a release asset's clothes, and the person who discovers it is broken is
# someone installing it as root on their master.
#
# build-release stays parameterised, so re-adding an architecture is one entry
# here plus a runner that can run its tests.
RELEASE_ARCHES := "amd64"

# Build every artefact for a release: binaries, tarballs, .deb, .rpm, checksums.
package: (build-release "linux" "amd64")
    #!/usr/bin/env bash
    set -euo pipefail
    cd dist
    for arch in {{RELEASE_ARCHES}}; do
        d="salt-events-linux-${arch}"
        install -m 0644 ../README.md ../LICENSE "${d}/"
        install -m 0644 ../docs/running.md "${d}/running.md"
        tar -czf "salt-events_{{VERSION}}_linux_${arch}.tar.gz" -C "${d}" .
    done
    cd ..
    # nfpm.yaml points at a fixed staging path, so each architecture is copied
    # into place in turn (see the note in nfpm.yaml).
    mkdir -p dist/pkgroot
    for arch in {{RELEASE_ARCHES}}; do
        install -m 0755 "dist/salt-events-linux-${arch}/salt-events" dist/pkgroot/salt-events
        for pkg in deb rpm; do
            VERSION="{{VERSION}}" ARCH="${arch}" \
                go tool nfpm package --config nfpm.yaml --packager "${pkg}" --target dist/
        done
    done
    rm -rf dist/pkgroot
    # sha256sum runs from inside dist/ so the file records bare names rather
    # than dist/-prefixed paths, which is what `sha256sum -c` expects after a
    # release asset is downloaded next to it.
    cd dist
    sha256sum *.tar.gz *.deb *.rpm > sha256sums.txt
    echo "artefacts in dist/:"
    ls -1

# Record real frames off the live bus into testdata (spec §13). Needs sudo.
capture n="200":
    sudo go run ./cmd/salt-events --capture={{n}} --capture-out=internal/saltipc/testdata/live-frames.bin
