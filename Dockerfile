# syntax=docker/dockerfile:1

# golang:1.27.1-trixie; keep in sync with go.mod.
FROM --platform=$BUILDPLATFORM golang@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS build

WORKDIR /src

# telesfor has no dependencies: there is no go.sum, and nothing to download.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /telesfor ./cmd/telesfor

# alpine:3.24.2 supplies ffmpeg's runtime dependencies.
FROM alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

LABEL org.opencontainers.image.title="telesfor" \
      org.opencontainers.image.description="A virtual TV tuner for Plex: live TV from streaming providers as an HDHomeRun network tuner." \
      org.opencontainers.image.source="https://github.com/combor/telesfor" \
      org.opencontainers.image.licenses="BSD-3-Clause"

RUN apk add --no-cache ffmpeg

COPY --from=build /telesfor /usr/local/bin/telesfor
COPY LICENSE /usr/share/licenses/telesfor/LICENSE

EXPOSE 5004

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --start-interval=2s \
    CMD ["/usr/local/bin/telesfor", "-healthcheck"]

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/telesfor"]
