# syntax=docker/dockerfile:1
#
# Builds a small, cgo-free image for a Raspberry Pi. Go cross-compiles on the
# build machine (no emulation), so building on a Mac for the Pi is fast:
#
#   just docker-build-pi        # or:
#   docker buildx build --platform linux/arm64 -t thermal --load .
#
# Use linux/arm/v7 for 32-bit Raspberry Pi OS.

ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -ldflags="-s -w -X github.com/connordoman/thermal/internal/server.Version=$VERSION" \
      -o /out/thermal . && \
    mkdir -p /out/data

# Static binary, CA certificates for image URLs, and a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/thermal /thermal
COPY --from=build --chown=nonroot:nonroot /out/data /data
# A .env in the data volume is read too.
WORKDIR /data
ENV THERMAL_ADDR=:8080 THERMAL_DB=/data/thermal.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/thermal", "-healthcheck"]
ENTRYPOINT ["/thermal"]
