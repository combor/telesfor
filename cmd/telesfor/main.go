// Command telesfor is a virtual TV tuner for Plex. It presents live TV from
// streaming providers as an HDHomeRun network tuner.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

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
	healthcheck := flag.Bool("healthcheck", false,
		"ask the telesfor at the listen address whether it is on the air, and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("telesfor", version)
		return
	}
	if *healthcheck {
		if err := checkHealth(*listen); err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}
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

// checkHealth asks the telesfor that listens on the given address whether it is
// on the air. It is the health check of the container image.
func checkHealth(listen string) error {
	u, err := healthURL(listen)
	if err != nil {
		return err
	}
	// No proxy: the one in the environment is for the providers.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get(u)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: %s", resp.Status)
	}
	return nil
}

// healthURL is where to ask a telesfor that listens on the given address. One
// that listens on every address is asked on loopback.
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("-listen: %w", err)
	}
	switch ip := net.ParseIP(host); {
	case host == "" || (ip != nil && ip.Equal(net.IPv4zero)):
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified():
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/lineup_status.json"}).String(), nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
