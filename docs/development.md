# Developing telesfor

[← Back to the README](../README.md)

For installation and command-line options, see the [usage guide](usage.md).

## How it works

telesfor presents streaming providers as an HDHomeRun network tuner. Plex
reads `/discover.json` and `/lineup.json` for the device and channels, and
`/xmltv.xml` for the guide. Tuning a channel opens its `/stream/…` URL.

```mermaid
flowchart LR
    provider["Streaming provider"] -->|"HLS through HTTP relay"| ffmpeg["ffmpeg"]
    ffmpeg -->|"MPEG-TS with aligned start"| plex["Plex"]
    provider -->|"Channels and programme listings"| tuner["Virtual tuner"]
    tuner -->|"Lineup and XMLTV guide"| plex
```

| Package | Job |
|---|---|
| `internal/provider` | The contract every TV source implements. |
| `internal/provider/tvp` | TVP channels, guide and stream URLs. |
| `internal/remux` | HTTP relay, timestamp repair, ffmpeg stream copy and startup alignment. |
| `internal/tuner` | HDHomeRun emulation, streaming endpoints and the XMLTV guide. |
| `cmd/telesfor` | Configuration and provider registration. |

## Streaming details

### Why a relay?

Providers hand out stream URLs that only work from the address that asked for
them, so the stream must leave through the same proxy as the API calls.
ffmpeg fails to tunnel HTTPS through some HTTP proxies. The relay handles
upstream requests with the provider's HTTP client instead.

Each upstream server gets a loopback counterpart. ffmpeg reads from that
local address, and the relay fetches the same path from the real server.
Each provider can have its own proxy, headers or cookies without ffmpeg
needing to know about them.

### Why trim the start?

A stream with separate audio and video playlists, like TVP's, can begin with
a few seconds of video and no sound, and even one that begins with both
begins with video. Plex may give up with "Could not tune channel", or its
remux for the player may crash. telesfor passes ffmpeg's output on from the
first keyframe that the audio has started before, with that audio in front.
This usually drops the first segment of the head start.

### Why repair timestamps?

Some TVP streams stamp frames to be decoded after they are due on screen.
ffmpeg replaces those times with guesses, resulting in uneven playback.
The relay moves the decoding times of affected segments back into order
before ffmpeg reads them. Segments with valid timestamps pass through unchanged.

### Why start behind the live edge?

Live streams arrive a segment at a time. Small delays in finding each new
segment can leave Plex's player without enough data. telesfor joins six
segments behind the newest and sends those segments immediately, giving the
player a buffer. This is typically 12 seconds on TVP channels, or 24 seconds
on channels with longer segments, at the cost of that much live delay.

## Adding a provider

Implement the four methods of `provider.Provider` in a package under
`internal/provider`:

```go
type Provider interface {
    Name() string
    Channels(ctx context.Context) ([]Channel, error)
    Programmes(ctx context.Context, channels []Channel, from, to time.Time) ([]Programme, error)
    Stream(ctx context.Context, channelID string) (Source, error)
}
```

Then add it to the list in `cmd/telesfor/main.go`. Its channels join the lineup
after those of the providers before it. Each stream returns a `Source` with
its URL and the HTTP client used to fetch it.

## Tests

With ffmpeg installed, run:

```sh
go vet ./...
go test -race ./...
```

CI runs vet and tests with the race detector on every push, checks Go
formatting and scans for known vulnerabilities.

The tests use recorded responses. To check the provider against TVP's real
API, which CI does daily:

```sh
TELESFOR_LIVE=1 go test -count=1 -v -run TestLive ./internal/provider/tvp
```

To test the container image, which CI also does on every push:

```sh
docker build -t telesfor:smoke .
TELESFOR_SMOKE_IMAGE=telesfor:smoke go test -count=1 -run TestContainerServesLineup ./cmd/telesfor
```

To test the Linux packages, which CI does as well. It needs GoReleaser and
Docker:

```sh
goreleaser release --snapshot --clean --skip=nix
TELESFOR_SMOKE_DIST=$PWD/dist go test -count=1 -timeout 20m -run TestPackageService ./cmd/telesfor
```

## Releases

Push a tag that starts with `v`, such as `v0.1.0`. Once the checks pass, CI
builds the archives and packages with GoReleaser and publishes them as a
GitHub release, to the AUR and to the Homebrew, Scoop and Nix repositories,
then pushes the container image to `ghcr.io/combor/telesfor`.
