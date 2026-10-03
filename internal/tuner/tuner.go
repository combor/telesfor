// Package tuner makes telesfor look like an HDHomeRun network tuner, the kind
// of device Plex knows how to use for Live TV & DVR.
//
// It serves the handful of endpoints Plex asks for: discover.json and
// lineup.json describe the device and its channels, xmltv.xml carries the TV
// guide, and /stream/… is a channel as MPEG-TS.
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
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/remux"
)

const (
	deviceID   = "7E1E5F04" // any 8 hex digits; Plex tells tuners apart by them
	tunerCount = 4          // how many streams Plex may open at once
)

// Tuner is an http.Handler that emulates an HDHomeRun.
type Tuner struct {
	providers []provider.Provider
	lineup    []channel
	remux     *remux.Remuxer
	mux       *http.ServeMux
}

// channel is a provider's channel with its place in the lineup.
type channel struct {
	provider.Channel
	number   string            // GuideNumber in Plex, and the channel's id in the guide
	provider provider.Provider // where the channel comes from
}

// New asks the providers for their channels and returns a tuner that offers
// them all, numbered from 1 in the order given.
func New(ctx context.Context, providers []provider.Provider, remuxer *remux.Remuxer) (*Tuner, error) {
	t := &Tuner{providers: providers, remux: remuxer, mux: http.NewServeMux()}
	for _, p := range providers {
		channels, err := p.Channels(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range channels {
			number := strconv.Itoa(len(t.lineup) + 1)
			t.lineup = append(t.lineup, channel{c, number, p})
		}
	}

	t.mux.HandleFunc("GET /discover.json", t.discover)
	t.mux.HandleFunc("GET /lineup_status.json", t.lineupStatus)
	t.mux.HandleFunc("GET /lineup.json", t.lineupJSON)
	t.mux.HandleFunc("POST /lineup.post", t.scan)
	t.mux.HandleFunc("GET /stream/{provider}/{channel}", t.stream)
	t.mux.HandleFunc("GET /xmltv.xml", t.xmltv)
	return t, nil
}

// ServeHTTP implements http.Handler.
func (t *Tuner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	resp := &response{ResponseWriter: w}
	t.mux.ServeHTTP(resp, r)
	slog.Debug("request", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr,
		"status", cmp.Or(resp.status, http.StatusOK), "bytes", resp.sent, "took", since(began))
}

// Channels reports how many channels are in the lineup.
func (t *Tuner) Channels() int { return len(t.lineup) }

// discover describes the device. Plex reads it when the tuner is added.
func (t *Tuner) discover(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"FriendlyName":    "telesfor",
		"Manufacturer":    "Silicondust",
		"ModelNumber":     "HDTC-2US",
		"FirmwareName":    "hdhomeruntc_atsc",
		"FirmwareVersion": "20150826",
		"DeviceID":        deviceID,
		"DeviceAuth":      "telesfor",
		"TunerCount":      tunerCount,
		"BaseURL":         baseURL(r),
		"LineupURL":       baseURL(r) + "/lineup.json",
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

	entries := make([]entry, 0, len(t.lineup)) // an empty lineup is [], not null
	for _, ch := range t.lineup {
		entries = append(entries, entry{
			GuideNumber: ch.number,
			GuideName:   ch.Name,
			URL:         baseURL(r) + "/stream/" + ch.provider.Name() + "/" + url.PathEscape(ch.ID),
		})
	}
	writeJSON(w, entries)
}

// scan answers Plex's request to scan for channels. There is nothing to scan:
// the lineup is known from the start.
func (t *Tuner) scan(w http.ResponseWriter, r *http.Request) {}

// stream serves a channel as MPEG-TS for as long as the viewer stays connected.
func (t *Tuner) stream(w http.ResponseWriter, r *http.Request) {
	ch, ok := t.find(r.PathValue("provider"), r.PathValue("channel"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	tuned := time.Now()
	source, err := ch.provider.Stream(r.Context(), ch.ID)
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
	for _, ch := range t.lineup {
		if ch.provider.Name() == providerName && ch.ID == id {
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

// baseURL is the address the client reached the tuner at, so that the URLs it
// is handed work from wherever it is.
func baseURL(r *http.Request) string { return "http://" + r.Host }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// since is the time passed since t, rounded for the log.
func since(t time.Time) time.Duration { return time.Since(t).Round(time.Millisecond) }

// megabytes formats a number of bytes for the log.
func megabytes(n int64) string { return fmt.Sprintf("%.1fMB", float64(n)/1e6) }
