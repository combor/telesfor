# syntax=docker/dockerfile:1

# Keep in sync with go.mod.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build

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

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

LABEL org.opencontainers.image.title="telesfor" \
      org.opencontainers.image.description="A virtual TV tuner for Plex: live TV from streaming providers as an HDHomeRun network tuner." \
      org.opencontainers.image.source="https://github.com/combor/telesfor" \
      org.opencontainers.image.licenses="BSD-3-Clause"

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
