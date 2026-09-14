# syntax=docker/dockerfile:1
#
# linuxserver.io "docker mod" image for plex-api.
#
# Build from the REPO ROOT:
#   docker build -f deploy/Dockerfile.mod -t plex-api-mod:dev .
#
# Consume by publishing it to a registry that allows ANONYMOUS pulls and
# setting, on the linuxserver/plex container:
#   DOCKER_MODS=ghcr.io/<owner>/plex-api-mod:latest
#
# The mod is a FROM scratch image: at container start the linuxserver
# init-mods step downloads its layers and untars them onto /.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Module files first so dependency resolution caches independently of sources.
COPY go.mod go.su[m] ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/plex-api ./cmd/plex-api
# Assemble the complete overlay in ONE directory so the final image is ONE
# layer. The linuxserver mod loader downloads only the first layer of a mod
# image (verified against mod script 3.20250825), so a second COPY would be
# silently dropped.
RUN mkdir -p /rootfs/usr/local/bin \
 && cp -a /src/deploy/root/. /rootfs/ \
 && cp /out/plex-api /rootfs/usr/local/bin/plex-api \
 && chmod 0755 /rootfs/usr/local/bin/plex-api

FROM scratch

LABEL maintainer="eric"
LABEL org.opencontainers.image.title="plex-api-mod"
LABEL org.opencontainers.image.description="linuxserver.io docker mod: HTTP access to the Plex library database via the bundled Plex SQLite engine"
LABEL org.opencontainers.image.source="https://github.com/pwsh/plex_api"
# linuxserver mod conventions
LABEL org.linuxserver.mod="plex-api"
LABEL org.linuxserver.mod.base="plex"

# The overlay tree plus the binary, as a single layer. Everything here lands
# at / in the target container.
COPY --from=build /rootfs/ /
