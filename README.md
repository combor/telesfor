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
- **A 48-hour TV guide.** Programme listings come straight from TVP.
- **Original stream quality.** Up to 1080p, without transcoding in telesfor.

> [!NOTE]
> Most TVP channels need a Polish connection. From abroad, use an HTTP proxy
> with a Polish exit. Paid and DRM-protected channels are not supported.

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
`http://<telesfor-host>:5004/` lists both addresses, ready to copy.

1. In Plex, open **Settings → Live TV & DVR** and set up a new DVR.
2. Add the tuner manually as `http://<telesfor-host>:5004`.
   Plex does not discover telesfor automatically.
3. Once the channels appear, choose an **XMLTV** guide and enter
   `http://<telesfor-host>:5004/xmltv.xml`.
4. Check the channel mapping and finish setup.

Your channels and guide are now available in Plex's **Live TV** section.

## Documentation

- [Configuration](docs/usage.md#configuration): change the port, proxy or logging.
- [Troubleshooting](docs/usage.md#troubleshooting): help with setup, channels and the guide.
- [Development](docs/development.md): how streaming works, tests and adding a provider.

Found a bug or missing something? [Open an issue](https://github.com/combor/telesfor/issues).

## License

[BSD-3-Clause](LICENSE).
