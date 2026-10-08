<h1 align="center">telesfor</h1>

<p align="center">
  <strong>Live TV from streaming services, in Plex.</strong>
</p>

<p align="center">
  <a href="https://github.com/combor/telesfor/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/combor/telesfor/ci.yml?branch=main&amp;event=push&amp;style=flat-square&amp;label=CI" alt="CI status"></a>
  <a href="https://github.com/combor/telesfor/releases"><img src="https://img.shields.io/github/v/release/combor/telesfor?style=flat-square" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-BSD--3--Clause-blue?style=flat-square" alt="License: BSD-3-Clause"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#connect-plex">Connect Plex</a> ·
  <a href="docs/usage.md#configuration">Configuration</a> ·
  <a href="docs/usage.md#troubleshooting">Troubleshooting</a>
</p>

telesfor turns streaming channels into a virtual HDHomeRun tuner. Watch,
pause and record through Plex's Live TV & DVR, with a TV guide built in.
No tuner hardware is needed.

## What you get

- **TVP channels.** TVP 1, TVP 2, TVP Info, TVP Sport, TVP Kultura,
  the regional TVP3 stations and more.
- **Globoplay's free channels.** TV Globo, Futura and ge tv, with a
  [Globo account](docs/usage.md#globoplay).
- **EBC's channels.** TV Brasil, TV Brasil Internacional, Canal Gov and
  Canal Educação, [without an account](docs/usage.md#ebc).
- **TV Cultura's channels.** TV Cultura and Cultura Fast,
  [without an account](docs/usage.md#tv-cultura).
- **France Télévisions' channels.** France 2, France 3, France 4, France 5
  and franceinfo, [without an account](docs/usage.md#francetv).
- **TF1+'s channels.** TF1, TFX and TF1 Séries Films, with a
  [TF1+ account](docs/usage.md#tf1), and LCI without one.
- **A 48-hour TV guide.** Programme listings come straight from the
  providers, fetched ahead of time and refreshed through the day, so Plex
  never waits on them.
- **Original stream quality.** Up to 1080p without transcoding, in the best
  quality your connection keeps up with.

> [!NOTE]
> Most TVP channels need a Polish connection, Globoplay needs a Brazilian
> one, and most of France Télévisions' and TF1+'s channels need a French
> one. From abroad, use an HTTP proxy with an exit in that country. Paid and
> DRM-protected channels are not supported.

## Quick start

You need **Plex Media Server with a Plex Pass**, and **ffmpeg** installed on
the machine running telesfor. Check that `ffmpeg -version` works in your terminal.

**1. Download telesfor.** Get the archive for your system from
[Releases](https://github.com/combor/telesfor/releases) and unpack it.
To build it yourself, see [Build from source](docs/usage.md#build-from-source).
To run it as a container, see [Docker](docs/usage.md#docker).
To install it with a package manager, see [Packages](docs/usage.md#packages).

**2. Start it.** Open a terminal in the unpacked folder and run:

```sh
./telesfor
```

If you need a Polish proxy, start with its address instead:

```sh
./telesfor -tvp-proxy 'http://<proxy-host>:<port>'
```

Replace the proxy placeholder with your proxy's address. On Windows, use
`.\telesfor.exe` in place of `./telesfor`.

telesfor listens on port **5004**. Keep it running while you set up Plex,
watch TV or record programmes.

## Connect Plex

Use the IP address or hostname of the machine running telesfor wherever you
see `<telesfor-host>`. It must be reachable from Plex Media Server.
telesfor's [settings page](docs/usage.md#web-interface) at
`http://<telesfor-host>:5004/` has each provider's addresses, ready to copy.

Each provider is a tuner of its own, with its own guide.

| Provider | Tuner | XMLTV guide |
|---|---|---|
| TVP | `http://<telesfor-host>:5004` | `http://<telesfor-host>:5004/xmltv.xml` |
| Globoplay | `http://<telesfor-host>:5004/globo` | `http://<telesfor-host>:5004/globo/xmltv.xml` |
| EBC | `http://<telesfor-host>:5004/ebc` | `http://<telesfor-host>:5004/ebc/xmltv.xml` |
| TV Cultura | `http://<telesfor-host>:5004/cultura` | `http://<telesfor-host>:5004/cultura/xmltv.xml` |
| france.tv | `http://<telesfor-host>:5004/francetv` | `http://<telesfor-host>:5004/francetv/xmltv.xml` |
| TF1+ | `http://<telesfor-host>:5004/tf1` | `http://<telesfor-host>:5004/tf1/xmltv.xml` |

1. In Plex, open **Settings → Live TV & DVR** and set up a new DVR.
2. Add a provider's tuner manually by its address.
   Plex does not discover telesfor automatically.
3. Once the channels appear, choose an **XMLTV** guide and enter that
   provider's guide address.
4. Check the channel mapping and finish setup.
5. For each other provider, add its tuner to the same DVR as another device,
   with its own guide address.

Your channels and guide are now available in Plex's **Live TV** section, in
one list. Globoplay's channels are numbered from 1001, EBC's from 2001,
TV Cultura's from 3001, france.tv's from 4001 and TF1+'s from 5001, so they
follow TVP's.

## Documentation

- [Configuration](docs/usage.md#configuration): change the port, proxies or logging.
- [Troubleshooting](docs/usage.md#troubleshooting): help with setup, channels and the guide.
- [Development](docs/development.md): how streaming works, tests and adding a provider.

Found a bug or missing something? [Open an issue](https://github.com/combor/telesfor/issues).

## License

[BSD-3-Clause](LICENSE).
