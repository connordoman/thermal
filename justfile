# Run `just` to list recipes.

# 64-bit Raspberry Pi OS is arm64; use PI_ARCH=arm for 32-bit.
pi_arch := env("PI_ARCH", "arm64")
pi_platform := if pi_arch == "arm" { "linux/arm/v7" } else { "linux/arm64" }
pi_host := env("PI_HOST", "pi@pos.local")
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-s -w -X github.com/connordoman/thermal/internal/server.Version=" + version
# Build against the local escpos checkout until it is published.
escpos_context := if path_exists("../escpos/go.mod") == "true" { "--build-context escpos=../escpos" } else { "" }

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
    docker buildx build {{escpos_context}} --build-arg VERSION={{version}} -t thermal:latest --load .

# Build a Docker image for the Raspberry Pi
docker-build-pi:
    docker buildx build {{escpos_context}} --platform {{pi_platform}} --build-arg VERSION={{version}} -t thermal:latest --load .

# Run the local image without a printer on http://localhost:8080
docker-run:
    docker run --rm -it -p 8080:8080 -e ESCPOS_CONNECTION=discard -v thermal-dev-data:/data thermal:latest

# Copy the binary to the Pi and restart its systemd service
deploy-pi: build-pi
    scp bin/thermal-linux-{{pi_arch}} {{pi_host}}:/tmp/thermal
    ssh {{pi_host}} 'sudo install -m 755 /tmp/thermal /usr/local/bin/thermal && sudo systemctl restart thermal'

# Build the image, load it on the Pi and (re)start it with compose
docker-deploy-pi: docker-build-pi
    docker save thermal:latest | gzip | ssh {{pi_host}} 'gunzip | docker load'
    ssh {{pi_host}} 'mkdir -p thermal'
    scp compose.yaml {{pi_host}}:thermal/compose.yaml
    ssh {{pi_host}} 'cd thermal && docker compose up -d && docker image prune -f'

# Follow the logs on the Pi (the bootstrap key is printed on first start)
logs-pi:
    ssh {{pi_host}} 'cd thermal && docker compose logs -f'

# Remove build output and the dev database
clean:
    rm -rf bin dev.db dev.db-shm dev.db-wal
