# telesfor — Plex virtual tuner, working skeleton with a TVP provider

## Context

The repo is empty (`LICENSE`, one-line `README.md`). The goal is a small Go service that Plex
sees as a network TV tuner, with TV sources plugged in as providers. TVP is the first provider.
This plan covers a bare-bones skeleton that really streams TVP in Plex, nothing more.

Decisions already made with you: **ffmpeg stream copy** for remuxing, **XMLTV guide from TVP's API**.

## What the research established

**Channels (TVP's public API, 45 listed)**

| Group | Count | Channels |
|---|---|---|
| Streamable from a Polish IP | 41 | TVP 1, TVP 2, Info, Sport, Kultura, Kultura 2, Historia, Historia 2, ABC, ABC 2, Kobieta, Dokument, Nauka, Rozrywka, Polonia, World, Wilno, Alfa, Belsat, Barwy Szczęścia, Klan, Kryminały, Miłość, Muzyka i Koncerty, Parlament Sejm/Senat, 15 × TVP3 regional |
| Streamable from anywhere | 3 of those | TVP Info, TVP Polonia, TVP World |
| Paid (`ITEM_NOT_PAID`) | 2 | TVP Seriale, TVP HD |
| DRM-encrypted | 2 | "TVP Na dobre i na złe" (FAST channel), TVP3 Opole (FairPlay, at least today) |

**TVP API** (`https://vod.tvp.pl/api/products`, no login/cookies/user-agent needed; `platform=BROWSER` is mandatory)

| Call | Result |
|---|---|
| `GET /lives?platform=BROWSER&lang=PL` | `items[]`: `id`, `title`, `payable`, `loginRequired`, `liveType`, `logoImages["1x1"][0].url` (protocol-relative). Not geo-blocked. |
| `GET /{id}/videos/playlist?platform=BROWSER&videoType=LIVE` | 200 `{sources:{HLS:[{src}]}, drm?:{…}}`; 403 `{code:"GEOIP_FILTER_FAILED"}` / `"ITEM_NOT_PAID"`. Geo-blocked. |
| `GET /lives/programmes?platform=BROWSER&lang=PL&since=…&till=…&liveId[]=…` | Array of `{id,title,description,lead,since,till,live:{id}}`. Dates as `2006-01-02T15:04-0700`. Max span ~24 h per call (36 h → `LIVE_PROGRAMME_INVALID_TIMESPAN`); data ~8 days ahead; returns programmes overlapping the window. All channels × 24 h ≈ 7 MB, ~6 s. |

**Stream**: HLS master with 1080p50 / 576p50 / 288p50 H.264 + AAC, **fMP4 segments, audio in a separate rendition**, relative URIs, 2 s segments. No MPEG-TS flavour exists, so remuxing is unavoidable.

**Network constraints that shape the design**
- Stream tokens are bound to the caller's IP (it is encoded in the CDN path), and the CDN nodes given to Polish clients are unreachable from elsewhere: the API call and every media fetch must leave through the same Polish exit.
- ffmpeg cannot tunnel HTTPS through the gluetun proxy (gluetun answers `CONNECT` with `Transfer-Encoding: chunked`, ffmpeg aborts). curl and Go's HTTP client are fine. So **Go does all upstream HTTP**.
- Verified: `ffmpeg -c copy -f mpegts` turns TVP 1 into valid MPEG-TS (H.264 1080p50 + AAC) with ~2–3 s startup; the Polish exit delivers ~69 Mbit/s against a ~5 Mbit/s stream. ffprobe *hangs* on DRM streams, so DRM must be refused before ffmpeg starts.

**Prerequisites**: a current Go toolchain, ffmpeg on `PATH`, a Plex Media Server that can reach telesfor, and, for the geo-blocked channels, an HTTP proxy with a Polish exit (gluetun's built-in HTTP proxy is one option).

## Architecture

```
Plex ── /discover.json, /lineup.json, /xmltv.xml ──▶ tuner ──▶ provider (contract) ◀── tvp
Plex ── GET /stream/tvp/399697 ──▶ tuner
                                     │ 1. provider.Stream()  → tokenised HLS URL + the provider's http.Client
                                     │ 2. remux: ffmpeg -i http://127.0.0.1:<port>/…/master.m3u8 -c copy -f mpegts pipe:1
                                     │ 3. relay: ffmpeg's GETs → provider's http.Client (proxy) → TVP CDN
Plex ◀──────────── MPEG-TS ──────────┘ 4. ffmpeg stdout → HTTP response; viewer disconnect kills ffmpeg
```

Four packages, one job each, standard library only (no third-party modules):

```
cmd/telesfor/main.go           wiring: flags → providers → remuxer → tuner → http.ListenAndServe
internal/provider/provider.go  the plugin contract
internal/provider/tvp/tvp.go   TVP: channel list, guide, stream resolution
internal/remux/remux.go        ffmpeg stream copy to MPEG-TS
internal/remux/relay.go        loopback fetcher so ffmpeg never touches the network
internal/remux/align.go        starts the output where every stream in it has started
internal/tuner/tuner.go        HDHomeRun emulation + /stream handler
internal/tuner/xmltv.go        /xmltv.xml guide
```

Dependencies point one way: `main → tuner → {provider, remux}` and `main → tvp → provider`. `remux` knows nothing about providers.

**The plugin contract** (`internal/provider/provider.go`) — adding a provider means implementing this in `internal/provider/<name>` and adding one line to the list in `main.go`:

```go
type Provider interface {
    Name() string                                              // URL-safe id, e.g. "tvp"
    Channels(ctx context.Context) ([]Channel, error)           // called once at startup
    Programmes(ctx context.Context, channels []Channel, from, to time.Time) ([]Programme, error)
    Stream(ctx context.Context, channelID string) (Source, error) // called on every tune
}
type Channel   struct{ ID, Name, Logo string }
type Programme struct{ ChannelID, Title, Description string; Start, Stop time.Time }
type Source    struct{ URL string; Client *http.Client }       // manifest ffmpeg can read + client to fetch it with
```

Providers are stateless; each owns its `*http.Client`, which is what makes per-provider proxies (different countries per provider) fall out for free.

## Files to create

- **`go.mod`** — `go mod init github.com/combor/telesfor`. **`.gitignore`** — the built binary.
- **`internal/provider/provider.go`** — the contract above, documented.
- **`internal/provider/tvp/tvp.go`**
  - `New(proxy string) (*Provider, error)`: clones `http.DefaultTransport`, sets `Proxy` when given, 60 s client timeout.
  - `Channels`: keeps items where `!payable && !loginRequired && liveType != "FAST"` (→ 42 today), in API order; logo = `"https:" + logoImages["1x1"][0].url`.
  - `Stream`: returns `sources.HLS[0].src`; errors on a `drm` key ("DRM-protected") and maps `GEOIP_FILTER_FAILED` to a hint to set `-tvp-proxy`.
  - `Programmes`: one request per 24 h window, de-duplicated by programme id (windows overlap).
- **`internal/remux/relay.go`** — one relay per stream. Every upstream server the stream touches gets a twin: a listener on `127.0.0.1:0` that answers each request by fetching the same path from that server with the provider's client, so relative URIs of any shape resolve as they do upstream. Redirects are not followed but handed to ffmpeg, rewritten to the twin of their target, so ffmpeg resolves a redirected manifest's URIs as a player would (TVP's CDN for viewers outside Poland redirects the manifest to an edge server and serves media only from there).
- **`internal/remux/remux.go`** — `New()` checks `ffmpeg` is on `PATH`. `Copy(ctx, w, url, client)` opens a relay for the stream and runs
  `ffmpeg -hide_banner -nostdin -loglevel fatal -i <relay url> -c copy -f mpegts pipe:1` via `exec.CommandContext` (stdout → `w`, stderr → ours, `no_proxy=*` in its env).
- **`internal/remux/align.go`** — an `io.Writer` between ffmpeg and the viewer. ffmpeg picks the starting segment of the video and the audio playlist independently, so now and then (14% of tunes in one sample) the stream opens with 4 s of video and no audio, and Plex then fails with "Could not tune channel". The aligner reads the MPEG-TS tables, holds back the packets since the last keyframe, and passes the stream on once every listed stream has started. It gives up and passes everything through after 12 MB.
- **`internal/tuner/tuner.go`** — `New(ctx, providers, remuxer)` loads the lineup once; channel numbers are `1..N` in provider order. Routes on a Go 1.22-style `ServeMux`:
  `GET /discover.json`, `GET /lineup_status.json`, `GET /lineup.json`, `POST /lineup.post` (no-op), `GET /stream/{provider}/{channel}`, `GET /xmltv.xml`.
  `BaseURL` is derived from the request's `Host`, so no address configuration. Tune failures before the first byte return `503` with the reason.
- **`internal/tuner/xmltv.go`** — `encoding/xml` structs; 48 h of guide per request; XMLTV channel id = the lineup's `GuideNumber`, so guide and lineup always agree. A provider guide failure returns `502` (Plex keeps its previous guide).
- **`cmd/telesfor/main.go`** — flags with env fallbacks: `-listen` (`TELESFOR_LISTEN`, default `:5004`), `-tvp-proxy` (`TELESFOR_TVP_PROXY`), `-debug` (`TELESFOR_DEBUG`). Provider list is an explicit slice; `log/slog` logging: three lines per tune (tuning, on air with the startup time, released with duration and bytes sent), and with `-debug` every request, upstream fetch and ffmpeg warning.
- **`README.md`** — what it is, requirements (Go, ffmpeg), how to run, how to add it in Plex, how to add a provider, why the relay exists.
- **Tests** (compact, hermetic)
  - `tvp_test.go`: `httptest` server with trimmed real responses — channel filtering, HLS resolution, DRM and geo-block errors, 24 h windowing + de-dup.
  - `remux_test.go`: relay path mapping, byte ranges and redirects; one end-to-end `Copy` over an ffmpeg-generated test HLS behind a redirect (skipped when ffmpeg is absent).
  - `align_test.go`: hand-built packet sequences (streams starting together, video first, audio first, giving up), and a real ffmpeg stream whose audio starts 3 s late.
  - `tuner_test.go`: fake provider — JSON shapes, lineup numbering/URLs, XMLTV output, 404/503 paths.

## Deliberately left out (natural next steps)

SSDP auto-discovery (the address is typed into Plex once) · token refresh for very long sessions · sharing one upstream between several viewers · quality cap (ffmpeg picks 1080p) · guide caching and a longer horizon · persisted channel numbers · Dockerfile · rewriting manifests that use absolute URIs.

Known wart: TVP3 Opole stays in the lineup (nothing in the listing marks it as DRM) and answers `503` when tuned; it can be unticked in Plex.

## Verification

1. `go vet ./... && go test ./...`
2. Run `go run ./cmd/telesfor -tvp-proxy http://<proxy-host>:<port>`, pointing at the proxy with the Polish exit.
3. `curl -s localhost:5004/discover.json | jq`; `curl -s localhost:5004/lineup.json | jq length` → 42; `lineup_status.json` sane.
4. `curl -s localhost:5004/xmltv.xml` parses as XML, has 42 channels and programmes for the next 48 h.
5. `ffprobe http://127.0.0.1:5004/stream/tvp/399697` → `mpegts`, H.264 1920×1080@50 + AAC. `timeout 30 curl … | wc -c` produces steady data; afterwards `pgrep -x ffmpeg` is empty (disconnect cleans up).
6. `curl -i …/stream/tvp/399750` → fast `503` (DRM). Restart without `-tvp-proxy`: TVP 1 → `503` with the geo-block hint, TVP Info still plays.
7. In Plex (needs your hands on the UI): Settings → Live TV & DVR → Set Up Plex DVR → enter `http://127.0.0.1:5004` manually → channels appear → choose XMLTV and give `http://127.0.0.1:5004/xmltv.xml` → confirm mapping → play TVP 1.
