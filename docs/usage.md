# Using telesfor

[← Back to the README](../README.md)

For your first setup, follow [Quick start](../README.md#quick-start) and
[Connect Plex](../README.md#connect-plex).

## Configuration

Use command-line flags or environment variables. Flags take precedence.

| Flag | Environment variable | Default | What it does |
|---|---|---|---|
| `-listen` | `TELESFOR_LISTEN` | `:5004` | Address and port to listen on. |
| `-tvp-proxy` | `TELESFOR_TVP_PROXY` | Unset | HTTP proxy for TVP's channel list, guide and streams. |
| `-globo-proxy` | `TELESFOR_GLOBO_PROXY` | Unset | HTTP proxy for Globoplay's sign-in, guide and streams. |
| `-ebc-proxy` | `TELESFOR_EBC_PROXY` | Unset | HTTP proxy for EBC's guide and streams. |
| `-cultura-proxy` | `TELESFOR_CULTURA_PROXY` | Unset | HTTP proxy for TV Cultura's guide and streams. |
| `-data` | `TELESFOR_DATA` | `telesfor` in your user configuration directory | Directory that keeps the sign-ins to providers. |
| `-debug` | `TELESFOR_DEBUG` | Off | Log requests, upstream fetches and ffmpeg warnings. |
| `-version` | | | Print the version and exit. |
| `-healthcheck` | | | Check that the telesfor at the listen address answers, and exit. |

Any nonempty value of `TELESFOR_DEBUG` enables debug logging, including `0`
or `false`. Leave it unset to keep debug logging off, or pass `-debug=false`.

For example, to use a different port and a Polish proxy:

```sh
./telesfor -listen :5005 -tvp-proxy 'http://<proxy-host>:<port>'
```

Replace the proxy placeholder with your proxy's address. If you change the
port, use that port in both the tuner address and the XMLTV URL in Plex.
On Windows, use `.\telesfor.exe` in place of `./telesfor`.

### Watching from outside Poland

Most TVP channels require a Polish connection. Use `-tvp-proxy` with an HTTP
proxy that exits in Poland. telesfor sends both API requests and the video
through that proxy, because TVP ties stream URLs to the requesting IP address.

Not every VPN server in Poland will do. TVP's CDN refuses the addresses of VPN
providers, and TVP's own servers answer only some of them. If the log says
that TVP names no server of its own, or streams are refused, try another
server of the VPN.

Without a Polish connection, channel availability is limited. Subscription
channels are left out of the lineup, and DRM-protected streams cannot play.

If a provider's proxy is unset, the standard `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY` environment settings apply to it.

### Globoplay

TV Globo, Futura and ge tv are free with a Globo account. To sign in, open
the **Globoplay** tab of the [settings page](#web-interface) and choose
**Sign in**. telesfor shows a code. Enter it at the address shown, signed in
to your Globo account there, within five minutes. Globo may ask you to
complete your profile first.

The channels then join Globoplay's tuner, ready to
[add to Plex](../README.md#connect-plex). The sign-in is kept in the data
directory, so it outlasts restarts. If Globo stops accepting it, the settings
page says so, and you sign in again there.

Globoplay plays only in Brazil. From abroad, set `-globo-proxy` to an HTTP
proxy that exits in Brazil. TV Globo is the regional station of the place the
proxy exits in. If Globo still blocks the streams, try another exit.

### EBC

TV Brasil, TV Brasil Internacional, Canal Gov and Canal Educação need no
account, and play outside Brazil. EBC keeps some of its pages to Brazil. If
it does the same to the streams, set `-ebc-proxy` to an HTTP proxy that exits
there.

Only TV Brasil has a guide, and it is the guide of its broadcast: EBC leaves
out of the web stream what it has no rights to show online. Where EBC lists
no programme, or leaves one unnamed, the guide has the channel's name in its
place, an hour at a time, so that Plex can still play and record the channel.

### TV Cultura

TV Cultura and Cultura Fast need no account, and play outside Brazil. If TV
Cultura stops that, set `-cultura-proxy` to an HTTP proxy that exits there.

Only TV Cultura has a guide, the one on its site. Cultura Play lists
programmes for Cultura Fast, but the stream does not keep to them, so its
guide has the channel's name on every hour, as an EBC channel without a guide
has.

TV Cultura takes six to ten seconds to start, longer than other channels:
see [Streaming details](development.md#why-start-behind-the-live-edge).

## Build from source

Install the Go version listed in [go.mod](../go.mod), or a newer version, and
make sure ffmpeg is on `PATH`. Download or clone this repository, then run
these commands from its root:

```sh
go build ./cmd/telesfor
./telesfor
```

Add `-tvp-proxy` if you need a Polish connection. Go is only needed to build
telesfor; ffmpeg is needed whenever it runs.

## Docker

The image includes ffmpeg and is configured with the environment variables
above:

```sh
docker run -d --name telesfor --restart unless-stopped -p 5004:5004 \
  -v telesfor:/data \
  -e TELESFOR_TVP_PROXY='http://<proxy-host>:<port>' \
  ghcr.io/combor/telesfor:latest
```

The volume keeps the sign-ins when the container is replaced.
Leave out the `-e` line if you do not need a proxy. To change the listen
address, set `TELESFOR_LISTEN` rather than `-listen`: the image's health check
cannot read flags.

## Packages

Each of these installs ffmpeg too.

| System | Install |
|---|---|
| Arch Linux | `telesfor-bin` from the AUR |
| Debian, Ubuntu | The `.deb` from [Releases](https://github.com/combor/telesfor/releases/latest) |
| Fedora | The `.rpm` from [Releases](https://github.com/combor/telesfor/releases/latest) |
| macOS | `brew install --cask combor/tap/telesfor` |
| Windows | `scoop bucket add combor https://github.com/combor/scoop-bucket`, then `scoop install combor/telesfor` |
| Nix | `nix-env -f https://github.com/combor/nur/archive/main.tar.gz -iA telesfor` |

The Linux packages install telesfor as a systemd service. Put its settings in
`/etc/telesfor/telesfor.env`, then start it and enable it at boot:

```sh
sudo systemctl enable --now telesfor
```

Restart it after changing settings. Its log is in `journalctl -u telesfor`,
and its data directory is `/var/lib/telesfor`.

## Web interface

Open `http://<telesfor-host>:5004/` in a browser. The **Settings** page has a
tab for each provider: the tuner and guide addresses to enter in Plex, its
sign-in if it has one, as [Globoplay](#globoplay) does, its proxy, and its
channels, marking the ones being watched. The **Server** tab shows the other
settings telesfor was started with. The page refreshes every five seconds.

The addresses are where your browser reached telesfor. Behind a reverse
proxy, they are where the proxy reaches it.

The page asks for no sign-in to telesfor itself: anyone who can reach
telesfor can open it, and sign Globoplay in or out. It never shows the Globo
account, and leaves out a proxy's user name and password.

## Troubleshooting

| Problem | What to check |
|---|---|
| telesfor cannot find ffmpeg | Run `ffmpeg -version` from the same terminal. Install ffmpeg or add it to `PATH`. |
| Plex cannot find the tuner | Add it manually. From the Plex server, check that `http://<telesfor-host>:5004/discover.json` is reachable. |
| Some channels will not play | Check the log for the reason. A region block needs a connection in the provider's country; DRM-protected channels are not supported. |
| Globoplay has no channels | Sign in on the settings page. See [Globoplay](#globoplay). |
| An EBC or TV Cultura channel is unavailable | The broadcaster is not streaming it at the moment. A proxy or an account will not bring it back. |
| Plex shows a playback error after its DVR was set up again | Quit and reopen the Plex app. |
| The guide is missing | Check that `http://<telesfor-host>:5004/xmltv.xml` is reachable from Plex and selected as its XMLTV guide. |
| Playback stops when the terminal closes | Keep telesfor running for both viewing and scheduled recordings. |

Use `/discover.json` to check the tuner, `/lineup.json` to see the channels,
or `/xmltv.xml` for the guide. Globoplay's are under `/globo`, EBC's under
`/ebc` and TV Cultura's under `/cultura`. If you changed the port, update
these addresses too.

### Reading the log

Each successful tune reports three events:

| Event | What it tells you |
|---|---|
| `tuning` | A viewer asked for a channel. |
| `on air` | The first bytes were sent. `startup` is how long the viewer waited. |
| `released` | The viewer disconnected. `after` is the session duration, and `sent` is the amount of data delivered. |

A session that ends just after going on air usually means the viewer gave up
on it. If the viewer leaves before anything is sent, telesfor logs a warning.
If tuning or streaming fails, it logs an error with the reason.

To see more detail, add `-debug` to your usual command:

```sh
./telesfor -debug
```

This includes Plex requests, upstream fetches with their size and timing, and
ffmpeg warnings. Brief `Packet corrupt` or `Invalid NAL unit size` warnings at
startup can occur when ffmpeg drops unused stream qualities.

### Live delay

telesfor starts a few segments behind the live edge to give Plex a buffer.
This typically adds 12 to 24 seconds of delay before Plex's own buffering.
See [Streaming details](development.md#streaming-details) for the reasoning.
