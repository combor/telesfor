# syntax=docker/dockerfile:1

# Keep the Go version in sync with go.mod.
FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /telesfor ./cmd/telesfor

# Alpine supplies ffmpeg and its runtime dependencies.
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# /data keeps the sign-ins to providers. Mount a volume there to keep them
# when the container is replaced.
RUN apk add --no-cache ffmpeg && install -d -o 65532 -g 65532 -m 700 /data
ENV TELESFOR_DATA=/data
VOLUME /data

COPY --from=build /telesfor /usr/local/bin/telesfor
COPY LICENSE /usr/share/licenses/telesfor/LICENSE

EXPOSE 5004

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --start-interval=2s \
    CMD ["/usr/local/bin/telesfor", "-healthcheck"]

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/telesfor"]
