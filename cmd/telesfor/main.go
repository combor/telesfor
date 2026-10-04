// Command telesfor is a virtual TV tuner for Plex. It presents live TV from
// streaming providers as an HDHomeRun network tuner.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/tvp"
	"github.com/combor/telesfor/internal/remux"
	"github.com/combor/telesfor/internal/tuner"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("listen", envOr("TELESFOR_LISTEN", ":5004"),
		"address to listen on (env TELESFOR_LISTEN)")
	tvpProxy := flag.String("tvp-proxy", os.Getenv("TELESFOR_TVP_PROXY"),
		"HTTP proxy for TVP, which blocks most channels outside Poland (env TELESFOR_TVP_PROXY)")
	debug := flag.Bool("debug", os.Getenv("TELESFOR_DEBUG") != "",
		"also log every request, every upstream fetch and ffmpeg's warnings (env TELESFOR_DEBUG)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("telesfor", version)
		return
	}
	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if err := run(*listen, *tvpProxy); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(listen, tvpProxy string) error {
	tvpProvider, err := tvp.New(tvpProxy)
	if err != nil {
		return err
	}

	// Every TV source plugs in here.
	providers := []provider.Provider{
		tvpProvider,
	}

	remuxer, err := remux.New()
	if err != nil {
		return err
	}
	t, err := tuner.New(context.Background(), providers, remuxer)
	if err != nil {
		return err
	}

	slog.Info("telesfor is on the air", "version", version, "listen", listen, "channels", t.Channels())
	return http.ListenAndServe(listen, t)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
