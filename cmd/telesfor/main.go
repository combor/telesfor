// Command telesfor is a virtual TV tuner for Plex. It presents live TV from
// streaming providers as an HDHomeRun network tuner.
package main

import (
	"cmp"
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
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/cultura"
	"github.com/combor/telesfor/internal/provider/ebc"
	"github.com/combor/telesfor/internal/provider/francetv"
	"github.com/combor/telesfor/internal/provider/globo"
	"github.com/combor/telesfor/internal/provider/tf1"
	"github.com/combor/telesfor/internal/provider/tvp"
	"github.com/combor/telesfor/internal/provider/wppilot"
	"github.com/combor/telesfor/internal/remux"
	"github.com/combor/telesfor/internal/store"
	"github.com/combor/telesfor/internal/tuner"
	"github.com/combor/telesfor/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type source struct {
	key    string
	about  string
	open   func(proxy string, db *bolt.DB) (provider.Provider, error)
	device tuner.Device
}

// Every TV source plugs in here. Plex knows a tuner by its ID, path and
// numbers, so never change them. TVP's tuner is at the root, where Plex has
// known it since it was the only one.
var sources = []source{
	{"tvp", "TVP, which blocks most channels outside Poland", proxied(tvp.New),
		tuner.Device{ID: "7E1E5F04", Name: "TVP", First: 1}},
	{"globo", "Globoplay, which blocks its channels outside Brazil", stored(globo.New),
		tuner.Device{ID: "7E1E5F05", Name: "Globoplay", Path: "/globo", First: 1001}},
	{"ebc", "EBC, should it block its channels outside Brazil", proxied(ebc.New),
		tuner.Device{ID: "7E1E5F06", Name: "EBC", Path: "/ebc", First: 2001}},
	{"cultura", "TV Cultura, should it block its channels outside Brazil", proxied(cultura.New),
		tuner.Device{ID: "7E1E5F07", Name: "TV Cultura", Path: "/cultura", First: 3001}},
	{"francetv", "France Télévisions, which blocks most channels outside France", proxied(francetv.New),
		tuner.Device{ID: "7E1E5F08", Name: "france.tv", Path: "/francetv", First: 4001}},
	{"tf1", "TF1+, which blocks most channels outside France", stored(tf1.New),
		tuner.Device{ID: "7E1E5F09", Name: "TF1+", Path: "/tf1", First: 5001}},
	// WP Pilot plays an account three channels at once.
	{"wppilot", "WP Pilot, which blocks its channels outside Poland", stored(wppilot.New),
		tuner.Device{ID: "7E1E5F0A", Name: "WP Pilot", Path: "/wppilot", First: 6001, Tuners: 3}},
}

func main() {
	listen := flag.String("listen", cmp.Or(os.Getenv("TELESFOR_LISTEN"), ":5004"),
		"address to listen on (env TELESFOR_LISTEN)")
	proxies := make([]string, len(sources))
	for i, s := range sources {
		flag.StringVar(&proxies[i], s.flag(), os.Getenv(s.env()),
			"HTTP proxy for "+s.about+" (env "+s.env()+")")
	}
	data := flag.String("data", cmp.Or(os.Getenv("TELESFOR_DATA"), dataDir()),
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
	if err := run(*listen, *data, *debug, proxies); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

// run puts telesfor on the air, with proxies[i] for sources[i].
func run(listen, data string, debug bool, proxies []string) error {
	if data == "" {
		return errors.New("no home directory to keep sign-ins in: set -data")
	}
	db, err := store.Open(data)
	if err != nil {
		return err
	}
	defer db.Close()

	// Opened before any tuner fetches channels, so a bad proxy fails fast.
	providers := make([]provider.Provider, len(sources))
	for i, s := range sources {
		if providers[i], err = s.open(proxies[i], db); err != nil {
			return err
		}
	}

	remuxer, err := remux.New()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	ui := &web.Handler{Settings: settings(listen, data, debug), Version: version}
	channels := 0
	for i, s := range sources {
		t, err := tuner.New(context.Background(), providers[i], remuxer, s.device)
		if err != nil {
			return err
		}
		t.Register(mux)
		ui.Providers = append(ui.Providers, web.Provider{Tuner: t, Settings: []web.Setting{s.proxySetting(proxies[i])}})
		channels += len(t.Lineup())
	}
	ui.Register(mux)

	slog.Info("telesfor is on the air", "version", version, "listen", listen, "channels", channels)
	return http.ListenAndServe(listen, mux)
}

// dataDir is where telesfor keeps its data unless told otherwise, or empty for
// an account without a home.
func dataDir() string {
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

func (s source) flag() string { return s.key + "-proxy" }

func (s source) env() string { return "TELESFOR_" + strings.ToUpper(s.key) + "_PROXY" }

// proxySetting is the source's proxy, for its tab of the settings page.
func (s source) proxySetting(proxy string) web.Setting {
	setting := web.Setting{Name: "Proxy", State: "Not set", Flag: "-" + s.flag(), Env: s.env()}
	if u, err := url.Parse(proxy); err == nil && u.Host != "" {
		u.User = nil // the page is open to whoever can reach the tuner
		setting.Value, setting.State = u.String(), ""
	}
	return setting
}

// proxied opens a provider that needs only its proxy.
func proxied[P provider.Provider](open func(proxy string) (P, error)) func(string, *bolt.DB) (provider.Provider, error) {
	return func(proxy string, _ *bolt.DB) (provider.Provider, error) { return open(proxy) }
}

// stored opens a provider that keeps its sign-in in the store.
func stored[P provider.Provider](open func(proxy string, db *bolt.DB) (P, error)) func(string, *bolt.DB) (provider.Provider, error) {
	return func(proxy string, db *bolt.DB) (provider.Provider, error) { return open(proxy, db) }
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
