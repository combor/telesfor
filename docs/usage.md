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
| `-francetv-proxy` | `TELESFOR_FRANCETV_PROXY` | Unset | HTTP proxy for France Télévisions' guide and streams. |
| `-tf1-proxy` | `TELESFOR_TF1_PROXY` | Unset | HTTP proxy for TF1+'s sign-in, guide and streams. |
| `-wppilot-proxy` | `TELESFOR_WPPILOT_PROXY` | Unset | HTTP proxy for WP Pilot's sign-in, guide and streams. |
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

### france.tv

France 2, France 3, France 4, France 5 and franceinfo need no account. All
but franceinfo play only in France. From abroad, set `-francetv-proxy` to an
HTTP proxy that exits there.

France 3 is the national channel, without the regional programmes. A film
that france.tv also has in its original language plays in French.

The guide is france.tv's own, under the names of the programmes: an
episode's name is the first line of its description. France Télévisions
lists a run of short episodes, as of the children's series on France 4, at
times that can be minutes off.

### TF1+

TF1, TFX and TF1 Séries Films are free with a TF1+ account, and LCI needs
none. To sign in, open the **TF1+** tab of the [settings page](#web-interface)
and choose **Sign in**. telesfor shows a code, and a link that fills it in.
Follow the link within five minutes, signed in to your TF1+ account there.

The channels then join LCI on TF1+'s tuner, ready to
[add to Plex](../README.md#connect-plex). The sign-in is kept in the data
directory, and telesfor renews it as it goes. If TF1 stops accepting it, the
settings page says so, and you sign in again there.

All but LCI play only in France. From abroad, set `-tf1-proxy` to an HTTP
proxy that exits there.

TMC is left out: TF1+ streams it DRM-protected. A programme that TF1+ also
has in its original language, or with audio description, plays in French.

The guide is the one TF1 publishes for the press. It has the night as one
programme, and nothing for LCI, whose guide has the channel's name on every
hour, as an EBC channel without a guide has.

### WP Pilot

WP Pilot gives a free account Polsat, TV 4, Telewizja WP and about thirty
channels more. To sign in, open the **WP Pilot** tab of the
[settings page](#web-interface) and choose **Sign in**. telesfor shows a
code. Enter it at the address shown, signed in to your WP Pilot account
there, within fifteen minutes.

WP Pilot plays a free account nothing until it has accepted the consents at
`pilot.wp.pl/ustawienia/zgody-rodo/`. The settings page says so while they
are missing.

The channels then appear on WP Pilot's tuner, ready to
[add to Plex](../README.md#connect-plex). The sign-in is kept in the data
directory, so it outlasts restarts. If WP stops accepting it, the settings
page says so, and you sign in again there.

The tuner has the channels WP Pilot lists as free for the account, without
its radio stations and without TVP's, which TVP's own tuner has. WP's list
does not tell which of them are DRM-protected: such a channel leaves the
tuner the first time it is tuned, until you sign out. Most channels come in
576p, a few in 1080p.

WP Pilot plays only in Poland. From abroad, set `-wppilot-proxy` to an HTTP
proxy that exits there. It refuses some channels, TVN among them, to the
addresses of VPNs.

An account plays three channels at once, on telesfor and in WP Pilot's own
apps together. A channel that plays on telesfor stops if the same channel is
opened and then closed elsewhere on the account. WP Pilot also counts the
changes of channel a free account makes, and starts it with 250.

The guide reaches about half a day ahead. Beyond that it has the channel's
name on every hour, as an EBC channel without a guide has.

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
telesfor can open it, and sign Globoplay, TF1+ or WP Pilot in or out. It
never shows the accounts, and leaves out a proxy's user name and password.

## Troubleshooting

| Problem | What to check |
|---|---|
| telesfor cannot find ffmpeg | Run `ffmpeg -version` from the same terminal. Install ffmpeg or add it to `PATH`. |
| Plex cannot find the tuner | Add it manually. From the Plex server, check that `http://<telesfor-host>:5004/discover.json` is reachable. |
| Some channels will not play | Check the log for the reason. A region block needs a connection in the provider's country; DRM-protected channels are not supported. |
| Globoplay has no channels | Sign in on the settings page. See [Globoplay](#globoplay). |
| TF1+ has LCI only | Sign in on the settings page. See [TF1+](#tf1). |
| WP Pilot has no channels | Sign in on the settings page. See [WP Pilot](#wp-pilot). |
| An EBC or TV Cultura channel is unavailable | The broadcaster is not streaming it at the moment. A proxy or an account will not bring it back. |
| Plex shows a playback error after its DVR was set up again | Quit and reopen the Plex app. |
| The guide is missing | Check that the provider's [guide address](../README.md#connect-plex) is reachable from Plex and selected as its XMLTV guide. |
| Playback stops when the terminal closes | Keep telesfor running for both viewing and scheduled recordings. |

Use `/discover.json` to check a tuner, `/lineup.json` to see its channels,
or `/xmltv.xml` for its guide, under its address in
[Connect Plex](../README.md#connect-plex). If you changed the port, update
these addresses too.

### Reading the log

Each successful tune reports three events:

| Event | What it tells you |
|---|---|
| `tuning` | A viewer asked for a channel. |
| `on air` | The first bytes were sent. `startup` is how long the viewer waited. |
| `quality` | The quality the stream starts in or changes to, and why. `speed` is what the connection was measured at, and `reserve` an estimate of what the player has in hand. |
| `released` | The viewer disconnected. `after` is the session duration, and `sent` is the amount of data delivered. |

A session that ends just after going on air usually means the viewer gave up
on it. If the viewer leaves before anything is sent, telesfor logs a warning.
If tuning or streaming fails, it logs an error with the reason.

To see more detail, add `-debug` to your usual command:

```sh
./telesfor -debug
```

This includes Plex requests, upstream fetches with their size and timing, and
ffmpeg warnings. `HTTP error 404` and `Failed to open segment` warnings at a
change of quality are how one ffmpeg is told to stop for the next.

### Picture quality

telesfor plays the best quality that the connection to the provider keeps up
with. It steps down before the picture freezes, and back up when there is
room. There is nothing to set: the `quality` lines of the log tell what
happened and why.
See [Why change quality?](development.md#why-change-quality) for the rules.

### Live delay

telesfor starts a few segments behind the live edge to give Plex a buffer.
This typically adds 12 to 24 seconds of delay before Plex's own buffering,
and 40 to 46 on France Télévisions' channels.
See [Streaming details](development.md#streaming-details) for the reasoning.
