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
	"github.com/combor/telesfor/internal/provider/cultura"
	"github.com/combor/telesfor/internal/provider/ebc"
	"github.com/combor/telesfor/internal/provider/francetv"
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
	ebcProxy := flag.String("ebc-proxy", os.Getenv("TELESFOR_EBC_PROXY"),
		"HTTP proxy for EBC, should it block its channels outside Brazil (env TELESFOR_EBC_PROXY)")
	culturaProxy := flag.String("cultura-proxy", os.Getenv("TELESFOR_CULTURA_PROXY"),
		"HTTP proxy for TV Cultura, should it block its channels outside Brazil (env TELESFOR_CULTURA_PROXY)")
	francetvProxy := flag.String("francetv-proxy", os.Getenv("TELESFOR_FRANCETV_PROXY"),
		"HTTP proxy for France Télévisions, which blocks most channels outside France (env TELESFOR_FRANCETV_PROXY)")
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
	if err := run(*listen, *tvpProxy, *globoProxy, *ebcProxy, *culturaProxy, *francetvProxy, *data, *debug); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(listen, tvpProxy, globoProxy, ebcProxy, culturaProxy, francetvProxy, data string, debug bool) error {
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
	ebcProvider, err := ebc.New(ebcProxy)
	if err != nil {
		return err
	}
	culturaProvider, err := cultura.New(culturaProxy)
	if err != nil {
		return err
	}
	francetvProvider, err := francetv.New(francetvProxy)
	if err != nil {
		return err
	}

	// Every TV source plugs in here, with a tuner of its own and the settings
	// it was started with, for its tab of the settings page. TVP's tuner is at
	// the root, where Plex has known it since it was the only one.
	sources := []struct {
		provider.Provider
		tuner.Device
		settings []web.Setting
	}{
		{tvpProvider, tuner.Device{ID: "7E1E5F04", Name: "TVP", First: 1},
			[]web.Setting{proxySetting(tvpProxy, "-tvp-proxy", "TELESFOR_TVP_PROXY")}},
		{globoProvider, tuner.Device{ID: "7E1E5F05", Name: "Globoplay", Path: "/globo", First: 1001},
			[]web.Setting{proxySetting(globoProxy, "-globo-proxy", "TELESFOR_GLOBO_PROXY")}},
		{ebcProvider, tuner.Device{ID: "7E1E5F06", Name: "EBC", Path: "/ebc", First: 2001},
			[]web.Setting{proxySetting(ebcProxy, "-ebc-proxy", "TELESFOR_EBC_PROXY")}},
		{culturaProvider, tuner.Device{ID: "7E1E5F07", Name: "TV Cultura", Path: "/cultura", First: 3001},
			[]web.Setting{proxySetting(culturaProxy, "-cultura-proxy", "TELESFOR_CULTURA_PROXY")}},
		{francetvProvider, tuner.Device{ID: "7E1E5F08", Name: "france.tv", Path: "/francetv", First: 4001},
			[]web.Setting{proxySetting(francetvProxy, "-francetv-proxy", "TELESFOR_FRANCETV_PROXY")}},
	}

	remuxer, err := remux.New()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	ui := &web.Handler{Settings: settings(listen, data, debug), Version: version}
	channels := 0
	for _, source := range sources {
		t, err := tuner.New(context.Background(), source.Provider, remuxer, source.Device)
		if err != nil {
			return err
		}
		t.Register(mux)
		ui.Providers = append(ui.Providers, web.Provider{Tuner: t, Settings: source.settings})
		channels += len(t.Lineup())
	}
	ui.Register(mux)

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

// settings is how telesfor itself was started, for the settings page.
func settings(listen, data string, debug bool) []web.Setting {
	logging := "Off"
	if debug {
		logging = "On"
	}
	return []web.Setting{
		{Name: "Listen address", Value: listen, Flag: "-listen", Env: "TELESFOR_LISTEN"},
		{Name: "Data directory", Value: data, Flag: "-data", Env: "TELESFOR_DATA"},
		{Name: "Debug logging", State: logging, Flag: "-debug", Env: "TELESFOR_DEBUG"},
	}
}

// proxySetting is a provider's proxy, for its tab of the settings page.
func proxySetting(proxy, flag, env string) web.Setting {
	setting := web.Setting{Name: "Proxy", State: "Not set", Flag: flag, Env: env}
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
