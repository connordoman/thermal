# Run `just` to list recipes.

# 64-bit Raspberry Pi OS is arm64; use PI_ARCH=arm for 32-bit.
pi_arch := env("PI_ARCH", "arm64")
pi_platform := if pi_arch == "arm" { "linux/arm/v7" } else { "linux/arm64" }
pi_host := env("PI_HOST", "pi@pos.local")
pi_url := env("PI_URL", "http://pos.local:8080")
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-s -w -X github.com/connordoman/thermal/internal/server.Version=" + version

[private]
default:
    @just --list

# Build for this machine (on macOS this includes libusb)
build:
    go build -ldflags "{{ldflags}}" -o bin/thermal .

# Build for Apple silicon Macs, with libusb (brew install libusb)
build-mac:
    CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -ldflags "{{ldflags}}" -o bin/thermal-darwin-arm64 .

# Build a static binary for the Raspberry Pi (no cgo)
build-pi:
    CGO_ENABLED=0 GOOS=linux GOARCH={{pi_arch}} GOARM=7 go build -trimpath -ldflags "{{ldflags}}" -o bin/thermal-linux-{{pi_arch}} .

# Run the tests
test *args:
    go test ./... {{args}}

# Check formatting, vet and test
check:
    test -z "$(gofmt -l .)" || (gofmt -l . && exit 1)
    go vet ./...
    go test ./...

# Format the code
fmt:
    gofmt -w .

# Regenerate database code after changing queries or migrations
generate:
    sqlc generate

# Run the server
run *args:
    go run . {{args}}

# Run with debug logging and a separate dev database
dev *args:
    THERMAL_DEBUG=true THERMAL_DB=dev.db go run . {{args}}

# Run without a printer: jobs are recorded but not printed
dev-discard *args:
    THERMAL_DEBUG=true THERMAL_DB=dev.db ESCPOS_CONNECTION=discard go run . {{args}}

# Build a Docker image for this machine
docker-build:
    docker buildx build --build-arg VERSION={{version}} -t thermal:latest --load .

# Build a Docker image for the Raspberry Pi
docker-build-pi:
    docker buildx build --platform {{pi_platform}} --build-arg VERSION={{version}} -t thermal:latest --load .

# Run the local image without a printer on http://localhost:8080
docker-run:
    docker run --rm -it -p 8080:8080 -e ESCPOS_CONNECTION=discard -v thermal-dev-data:/data thermal:latest

# Tag a release (e.g. just release v0.2.0); CI publishes it and the Pi updates within minutes
release version: check
    #!/usr/bin/env sh
    set -eu
    case "{{version}}" in v[0-9]*) ;; *) echo "use a version like v0.2.0" >&2; exit 1 ;; esac
    test -z "$(git status --porcelain)" || { echo "commit your changes first" >&2; exit 1; }
    # CI has no ../escpos, so make sure the published modules build.
    GOWORK=off go build -o /dev/null .
    git tag -a "{{version}}" -m "{{version}}"
    git push origin "{{version}}"
    echo "Watch the build: gh run watch"

# Show the version the Pi is running
pi-version:
    curl -fsS {{pi_url}}/healthz; echo

# Set up the Pi over SSH (or run deploy/install.sh on it yourself)
pi-install:
    ssh -t {{pi_host}} 'curl -fsSL https://raw.githubusercontent.com/connordoman/thermal/main/deploy/install.sh | sudo sh'

# Make the Pi check for an update now instead of within 5 minutes
pi-update:
    ssh -t {{pi_host}} 'sudo systemctl start thermal-update && sudo docker compose --project-directory /opt/thermal ps'

# Follow the logs on the Pi (the bootstrap key is printed on first start)
pi-logs:
    ssh -t {{pi_host}} 'sudo docker compose --project-directory /opt/thermal logs -f'

# Copy a binary to the Pi and restart its systemd service (without Docker)
deploy-pi: build-pi
    scp bin/thermal-linux-{{pi_arch}} {{pi_host}}:/tmp/thermal
    ssh {{pi_host}} 'sudo install -m 755 /tmp/thermal /usr/local/bin/thermal && sudo systemctl restart thermal'

# Remove build output and the dev database
clean:
    rm -rf bin dev.db dev.db-shm dev.db-wal
