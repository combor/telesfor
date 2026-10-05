// Package tuner makes telesfor look like an HDHomeRun network tuner, the kind
// of device Plex knows how to use for Live TV & DVR.
//
// It serves the handful of endpoints Plex asks for: discover.json and
// lineup.json describe the device and its channels, xmltv.xml carries the TV
// guide, and /stream/… is a channel as MPEG-TS.
//
// Every provider gets a tuner of its own, with its own guide. Plex takes them
// as devices of one DVR and lists their channels together, sorted by number.
package tuner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/remux"
)

const tunerCount = 4 // how many streams Plex may open at once

// Device is what tells one tuner from another.
type Device struct {
	ID   string // any 8 hex digits; Plex tells tuners apart by them
	Name string // the provider's name for people, such as "TVP"
	Path string // where the tuner is served, such as "/globo"; empty for the root

	// First is the GuideNumber of the first channel. Plex sorts the channels
	// of all tuners by number, so ranges that do not overlap keep each
	// provider's channels together.
	First int
}

// Tuner is an http.Handler that emulates an HDHomeRun with the channels of a
// provider.
type Tuner struct {
	provider provider.Provider
	device   Device
	remux    *remux.Remuxer
	mux      *http.ServeMux
	scanning sync.Mutex                // one Scan at a time
	lineup   atomic.Pointer[[]channel] // replaced whole by Scan
}

// channel is a provider's channel with its place in the lineup.
type channel struct {
	provider.Channel
	number  string        // GuideNumber in Plex, and the channel's id in the guide
	streams *atomic.Int32 // how many streams of it are open
}

// New returns a tuner that offers the channels of p.
func New(ctx context.Context, p provider.Provider, remuxer *remux.Remuxer, device Device) (*Tuner, error) {
	t := &Tuner{provider: p, device: device, remux: remuxer, mux: http.NewServeMux()}
	if err := t.Scan(ctx); err != nil {
		return nil, err
	}
	if account, ok := p.(provider.Account); ok {
		account.OnChange(func() {
			if err := t.Scan(context.Background()); err != nil {
				slog.Error("scan failed", "provider", p.Name(), "err", err)
			}
		})
	}

	t.mux.HandleFunc("GET /discover.json", t.discover)
	t.mux.HandleFunc("GET /lineup_status.json", t.lineupStatus)
	t.mux.HandleFunc("GET /lineup.json", t.lineupJSON)
	t.mux.HandleFunc("POST /lineup.post", t.scan)
	t.mux.HandleFunc("GET /stream/{provider}/{channel}", t.stream)
	t.mux.HandleFunc("GET /xmltv.xml", t.xmltv)
	return t, nil
}

// Scan asks the provider for its channels and numbers them from the device's
// first number, by their place if the provider gives one and in the order
// given otherwise.
func (t *Tuner) Scan(ctx context.Context) error {
	// Two scans at once could publish their lineups in the wrong order.
	t.scanning.Lock()
	defer t.scanning.Unlock()
	channels, err := t.provider.Channels(ctx)
	if err != nil {
		return err
	}
	open := map[string]*atomic.Int32{} // a channel that stays keeps its viewers
	for _, ch := range t.channels() {
		open[ch.ID] = ch.streams
	}
	lineup := make([]channel, len(channels))
	for i, c := range channels {
		streams := open[c.ID]
		if streams == nil {
			streams = new(atomic.Int32)
		}
		place := i
		if c.Place > 0 {
			place = c.Place - 1
		}
		lineup[i] = channel{c, strconv.Itoa(t.device.First + place), streams}
	}
	t.lineup.Store(&lineup)
	return nil
}

func (t *Tuner) channels() []channel {
	if lineup := t.lineup.Load(); lineup != nil {
		return *lineup
	}
	return nil
}

// Register serves the tuner on mux, at its device's path.
func (t *Tuner) Register(mux *http.ServeMux) {
	mux.Handle(t.device.Path+"/", http.StripPrefix(t.device.Path, t))
}

// ServeHTTP implements http.Handler.
func (t *Tuner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	resp := &response{ResponseWriter: w}
	t.mux.ServeHTTP(resp, r)
	// Register strips the device's path, which the log needs to tell tuners apart.
	slog.Debug("request", "method", r.Method, "path", t.device.Path+r.URL.Path, "from", r.RemoteAddr,
		"status", cmp.Or(resp.status, http.StatusOK), "bytes", resp.sent, "took", since(began))
}

// Name is the provider's name for people.
func (t *Tuner) Name() string { return t.device.Name }

// Provider is where the tuner's channels come from.
func (t *Tuner) Provider() provider.Provider { return t.provider }

// URL is the address the client reached the tuner at, so that the URLs it is
// handed work from wherever it is.
func (t *Tuner) URL(r *http.Request) string { return t.Address(r.Host) }

// Address is where the tuner is on a host, which may come with a port.
func (t *Tuner) Address(host string) string { return "http://" + host + t.device.Path }

// Station is a channel of the lineup, and how many streams of it are open.
type Station struct {
	Number  string // GuideNumber in Plex
	Name    string
	Streams int
}

// Lineup lists the channels in the order Plex shows them.
func (t *Tuner) Lineup() []Station {
	lineup := t.channels()
	stations := make([]Station, len(lineup))
	for i, ch := range lineup {
		stations[i] = Station{ch.number, ch.Name, int(ch.streams.Load())}
	}
	return stations
}

// discover describes the device. Plex reads it when the tuner is added.
func (t *Tuner) discover(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"FriendlyName":    "telesfor " + t.device.Name,
		"Manufacturer":    "Silicondust",
		"ModelNumber":     "HDTC-2US",
		"FirmwareName":    "hdhomeruntc_atsc",
		"FirmwareVersion": "20150826",
		"DeviceID":        t.device.ID,
		"DeviceAuth":      "telesfor",
		"TunerCount":      tunerCount,
		"BaseURL":         t.URL(r),
		"LineupURL":       t.URL(r) + "/lineup.json",
	})
}

// lineupStatus tells Plex that the channel list is ready to be read.
func (t *Tuner) lineupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"ScanInProgress": 0,
		"ScanPossible":   1,
		"Source":         "Cable",
		"SourceList":     []string{"Cable"},
	})
}

// lineupJSON lists the channels and where to stream each of them from.
func (t *Tuner) lineupJSON(w http.ResponseWriter, r *http.Request) {
	type entry struct{ GuideNumber, GuideName, URL string }

	lineup := t.channels()
	entries := make([]entry, 0, len(lineup)) // an empty lineup is [], not null
	for _, ch := range lineup {
		entries = append(entries, entry{
			GuideNumber: ch.number,
			GuideName:   ch.Name,
			URL:         t.URL(r) + "/stream/" + t.provider.Name() + "/" + url.PathEscape(ch.ID),
		})
	}
	writeJSON(w, entries)
}

// scan answers Plex's request to scan for channels. There is nothing to scan:
// the lineup is known already.
func (t *Tuner) scan(w http.ResponseWriter, r *http.Request) {}

// stream serves a channel as MPEG-TS for as long as the viewer stays connected.
func (t *Tuner) stream(w http.ResponseWriter, r *http.Request) {
	ch, ok := t.find(r.PathValue("provider"), r.PathValue("channel"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	tuned := time.Now()
	source, err := t.provider.Stream(r.Context(), ch.ID)
	if err != nil {
		slog.Error("tune failed", "channel", ch.Name, "viewer", r.RemoteAddr, "err", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	if r.Method == http.MethodHead {
		return // a probe learns that the channel can be tuned, without tuning it
	}

	slog.Info("tuning", "channel", ch.Name, "viewer", r.RemoteAddr)
	ch.streams.Add(1)
	defer ch.streams.Add(-1)
	out := &broadcast{ResponseWriter: w, channel: ch.Name, tuned: tuned}
	err = t.remux.Copy(r.Context(), out, source.URL, source.Client)

	// How a stream ended says most about what went wrong, if anything did.
	stats := []any{"channel", ch.Name, "after", since(tuned), "sent", megabytes(out.sent)}
	switch {
	case r.Context().Err() != nil && out.sent == 0:
		slog.Warn("viewer left before the stream started", stats...)
	case r.Context().Err() != nil:
		slog.Info("released", stats...)
	case err != nil:
		slog.Error("stream failed", append(stats, "err", err)...)
		if out.sent == 0 {
			http.Error(w, "stream failed", http.StatusServiceUnavailable)
		}
	default:
		slog.Info("stream ended", stats...)
	}
}

// find looks a channel up by its provider's name and its id within the provider.
func (t *Tuner) find(providerName, id string) (channel, bool) {
	if providerName != t.provider.Name() {
		return channel{}, false
	}
	for _, ch := range t.channels() {
		if ch.ID == id {
			return ch, true
		}
	}
	return channel{}, false
}

// broadcast is the body of a stream on its way to a viewer. It counts what is
// sent, and announces the first byte: the moment the channel is on air.
type broadcast struct {
	http.ResponseWriter
	channel string
	tuned   time.Time // when the viewer asked for the channel
	sent    int64
}

func (b *broadcast) Write(p []byte) (int, error) {
	if b.sent == 0 {
		slog.Info("on air", "channel", b.channel, "startup", since(b.tuned))
	}
	n, err := b.ResponseWriter.Write(p)
	b.sent += int64(n)
	return n, err
}

// response notes the status and size of a response, for the request log.
type response struct {
	http.ResponseWriter
	status int // 0 until the header is sent
	sent   int64
}

func (r *response) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *response) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.sent += int64(n)
	return n, err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// since is the time passed since t, rounded for the log.
func since(t time.Time) time.Duration { return time.Since(t).Round(time.Millisecond) }

// megabytes formats a number of bytes for the log.
func megabytes(n int64) string { return fmt.Sprintf("%.1fMB", float64(n)/1e6) }
