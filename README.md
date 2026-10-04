# telesfor

A virtual TV tuner for Plex. telesfor presents live TV from streaming providers
as an HDHomeRun network tuner, so Plex's Live TV & DVR can show, pause and
record it.

TV sources plug in as providers. The first one is **TVP**, the Polish public
broadcaster.

## What you get

- About 40 TVP channels: TVP 1, TVP 2, TVP Info, TVP Sport, TVP Kultura,
  TVP Historia, TVP ABC, the regional TVP3 stations and more, in the quality
  TVP streams them (up to 1080p, no transcoding).
- A TV guide straight from TVP, 48 hours ahead.

TVP blocks most of its channels outside Poland. From abroad you need an HTTP
proxy with a Polish exit; without one only TVP Info, TVP Polonia and TVP World
play. Channels that need a subscription (TVP Seriale, TVP HD) are left out, and
the few that are DRM-encrypted cannot be played.

## Requirements

- `ffmpeg` on `PATH`
- Plex Media Server with Live TV & DVR, which needs a Plex Pass

## Run

Download the archive for your system from the
[releases](https://github.com/combor/telesfor/releases), unpack it and start
telesfor:

```sh
./telesfor -tvp-proxy http://<proxy-host>:<port>
```

Or build it yourself, with a recent Go toolchain:

```sh
go build ./cmd/telesfor
```

| Flag         | Environment          | Default | Meaning                                                    |
|--------------|----------------------|---------|------------------------------------------------------------|
| `-listen`    | `TELESFOR_LISTEN`    | `:5004` | address to listen on                                       |
| `-tvp-proxy` | `TELESFOR_TVP_PROXY` | none    | HTTP proxy for all TVP traffic                             |
| `-debug`     | `TELESFOR_DEBUG`     | off     | also log every request, upstream fetch and ffmpeg warning |
| `-version`   |                      |         | print the version and exit                                 |

## Add it to Plex

1. Open Settings → Live TV & DVR and set up a new DVR.
2. Plex does not find telesfor on its own. Enter the tuner's address by hand:
   `http://<telesfor-host>:5004`.
3. Continue once Plex has listed the channels.
4. For the guide, choose XMLTV and give `http://<telesfor-host>:5004/xmltv.xml`.
5. Check the channel mapping and finish.

## How it works

```
Plex ── /discover.json, /lineup.json, /xmltv.xml ──▶ tuner ──▶ provider ◀── tvp
Plex ── GET /stream/tvp/399697 ──▶ tuner
                                     │ 1. the provider resolves the channel to an HLS URL
                                     │ 2. ffmpeg remuxes it to MPEG-TS, reading through the relay
                                     │ 3. the relay fetches with the provider's HTTP client
Plex ◀──────────── MPEG-TS ──────────┘ 4. ffmpeg's output is the response
```

| Package                 | Job                                                         |
|-------------------------|-------------------------------------------------------------|
| `internal/provider`     | the contract every TV source implements                     |
| `internal/provider/tvp` | TVP: channels, guide, streams                               |
| `internal/remux`        | ffmpeg stream copy to MPEG-TS, fed through a loopback relay |
|                         | that repairs timestamps, and trimmed to start where all its |
|                         | streams have started                                        |
| `internal/tuner`        | HDHomeRun emulation and the XMLTV guide                     |
| `cmd/telesfor`          | wiring                                                      |

**Why a relay?** Providers hand out stream URLs that only work from the address
that asked for them, so the stream has to leave through the same proxy as the
API calls. ffmpeg cannot be trusted with that: it fails to tunnel HTTPS through
some HTTP proxies (gluetun's, for one). So ffmpeg never touches the network.
Every server a stream comes from gets a twin on loopback: ffmpeg reads
`http://127.0.0.1:<port>/…`, and the relay fetches the same path from the real
server with the provider's own HTTP client. Each provider can therefore have
its own proxy, headers or cookies, and ffmpeg needs to know about none of them.

**Why trim the start?** A stream whose audio and video come as separate
playlists, like TVP's, sometimes begins with a few seconds of video and no
sound. Players cope; Plex gives up on the channel ("Could not tune channel").
So telesfor passes ffmpeg's output on only from the point where every stream
has started, beginning at a keyframe.

**Why repair timestamps?** On a handful of channels, TVP stamps frames to be
decoded after they are due on screen, which cannot be done. ffmpeg replaces
those times with guesses, and writes a stream that is decoded in fits and
starts. So the relay moves the decoding times of such segments back into order
before ffmpeg reads them. Segments with sound timestamps pass through
untouched.

**Why start behind the live edge?** A live stream arrives a segment at a time,
and ffmpeg finds each new segment a little later than the one before, until it
is a whole segment behind and catches up. Plex keeps a few seconds of what it
receives back from its player, and shows a spinner when the player runs dry.
So telesfor joins a stream six segments behind its newest (12 s on most TVP
channels, 24 s on the rest) and hands those over at once: the player starts
with that much in hand. The price is a picture that much further behind the
broadcast.

## Reading the log

Every time a channel is tuned, telesfor logs three lines:

```
INFO tuning channel="TVP 1" viewer=127.0.0.1:53422
INFO on air channel="TVP 1" startup=1.9s
INFO released channel="TVP 1" after=24m3s sent=861.5MB
```

`startup` is how long the viewer waited for the first byte, and the last line
says how long the session lasted and how much was sent. A session that ends
seconds after going on air is one the viewer gave up on. If the viewer leaves
before anything was sent, the last line is a warning instead, and a channel
that cannot be tuned at all is an error with the reason.

Run with `-debug` to also see each request Plex makes, each file fetched from
the provider with its size and timing, and ffmpeg's own warnings. A few of
those are part of every tune: `Packet corrupt` and `Invalid NAL unit size`
right after `on air` are ffmpeg dropping the qualities it will not use, in the
middle of a frame.

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
after those of the providers before it.

## Development

```sh
go vet ./... && go test ./...
```

CI runs the same on every push, with ffmpeg installed and the race detector on,
and checks for known vulnerabilities.

The tests run on recorded responses. To check the provider against TVP's real
API, which CI does daily:

```sh
TELESFOR_LIVE=1 go test -count=1 -v -run TestLive ./internal/provider/tvp
```

To release, push a tag that starts with `v`, such as `v0.1.0`. Once the checks
pass, CI builds the archives with GoReleaser and publishes them as a GitHub
release.
