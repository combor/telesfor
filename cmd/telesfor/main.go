// Command telesfor is a virtual TV tuner for Plex. It presents live TV from
// streaming providers as an HDHomeRun network tuner.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/globo"
	"github.com/combor/telesfor/internal/provider/tvp"
	"github.com/combor/telesfor/internal/remux"
	"github.com/combor/telesfor/internal/store"
	"github.com/combor/telesfor/internal/tuner"
	"github.com/combor/telesfor/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("listen", envOr("TELESFOR_LISTEN", ":5004"),
		"address to listen on (env TELESFOR_LISTEN)")
	tvpProxy := flag.String("tvp-proxy", os.Getenv("TELESFOR_TVP_PROXY"),
		"HTTP proxy for TVP, which blocks most channels outside Poland (env TELESFOR_TVP_PROXY)")
	globoProxy := flag.String("globo-proxy", os.Getenv("TELESFOR_GLOBO_PROXY"),
		"HTTP proxy for Globoplay, which blocks its channels outside Brazil (env TELESFOR_GLOBO_PROXY)")
	data := flag.String("data", dataDir(),
		"directory to keep sign-ins in (env TELESFOR_DATA)")
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
	if err := run(*listen, *tvpProxy, *globoProxy, *data, *debug); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(listen, tvpProxy, globoProxy, data string, debug bool) error {
	if data == "" {
		return errors.New("no home directory to keep sign-ins in: set -data")
	}
	db, err := store.Open(data)
	if err != nil {
		return err
	}
	defer db.Close()

	tvpProvider, err := tvp.New(tvpProxy)
	if err != nil {
		return err
	}
	globoProvider, err := globo.New(globoProxy, db)
	if err != nil {
		return err
	}

	// Every TV source plugs in here, with a tuner of its own. TVP's is at the
	// root, where Plex has known it since it was the only one.
	sources := []struct {
		provider.Provider
		tuner.Device
	}{
		{tvpProvider, tuner.Device{ID: "7E1E5F04", Name: "TVP", First: 1}},
		{globoProvider, tuner.Device{ID: "7E1E5F05", Name: "Globoplay", Path: "/globo", First: 1001}},
	}

	remuxer, err := remux.New()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	var tuners []*tuner.Tuner
	channels := 0
	for _, source := range sources {
		t, err := tuner.New(context.Background(), source.Provider, remuxer, source.Device)
		if err != nil {
			return err
		}
		t.Register(mux)
		tuners = append(tuners, t)
		channels += len(t.Lineup())
	}
	(&web.Handler{Tuners: tuners, Settings: settings(listen, tvpProxy, globoProxy, data, debug), Version: version}).Register(mux)

	slog.Info("telesfor is on the air", "version", version, "listen", listen, "channels", channels)
	return http.ListenAndServe(listen, mux)
}

// dataDir is where telesfor keeps its data unless told otherwise, or empty for
// an account without a home.
func dataDir() string {
	if dir := os.Getenv("TELESFOR_DATA"); dir != "" {
		return dir
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "telesfor")
}

// settings is how telesfor was started, for the settings page.
func settings(listen, tvpProxy, globoProxy, data string, debug bool) []web.Setting {
	logging := "Off"
	if debug {
		logging = "On"
	}
	return []web.Setting{
		{Name: "Listen address", Value: listen, Flag: "-listen", Env: "TELESFOR_LISTEN"},
		proxySetting("TVP proxy", tvpProxy, "-tvp-proxy", "TELESFOR_TVP_PROXY"),
		proxySetting("Globoplay proxy", globoProxy, "-globo-proxy", "TELESFOR_GLOBO_PROXY"),
		{Name: "Data directory", Value: data, Flag: "-data", Env: "TELESFOR_DATA"},
		{Name: "Debug logging", State: logging, Flag: "-debug", Env: "TELESFOR_DEBUG"},
	}
}

func proxySetting(name, proxy, flag, env string) web.Setting {
	setting := web.Setting{Name: name, State: "Not set", Flag: flag, Env: env}
	if u, err := url.Parse(proxy); err == nil && u.Host != "" {
		u.User = nil // the page is open to whoever can reach the tuner
		setting.Value, setting.State = u.String(), ""
	}
	return setting
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
