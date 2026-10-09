package tuner

import (
	"bytes"
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// guideSpan is how far ahead the guide reaches.
const guideSpan = 48 * time.Hour

// The guide is fetched ahead of Plex asking for it: anew every guideRefresh,
// and after guideRetry when a fetch failed, waiting twice as long each
// failure in a row, up to guideRefresh.
const (
	guideRefresh = 6 * time.Hour
	guideRetry   = 10 * time.Minute
)

// xmltvTime is the timestamp format of XMLTV, as in 20261003173500 +0200.
const xmltvTime = "20060102150405 -0700"

// The XMLTV document: every channel, then every programme.
type (
	xmlTV struct {
		XMLName    xml.Name       `xml:"tv"`
		Generator  string         `xml:"generator-info-name,attr"`
		Channels   []xmlChannel   `xml:"channel"`
		Programmes []xmlProgramme `xml:"programme"`
	}
	xmlChannel struct {
		ID   string   `xml:"id,attr"`
		Name string   `xml:"display-name"`
		Icon *xmlIcon `xml:"icon"`
	}
	xmlIcon struct {
		Src string `xml:"src,attr"`
	}
	xmlProgramme struct {
		Start   string   `xml:"start,attr"`
		Stop    string   `xml:"stop,attr"`
		Channel string   `xml:"channel,attr"`
		Title   string   `xml:"title"`
		Desc    string   `xml:"desc,omitempty"`
		Icon    *xmlIcon `xml:"icon"`
	}
)

// guide is a rendered XMLTV document, ready to serve. It carries the lineup
// it was rendered from, whose numbers name its channels — the guide of
// another lineup is stale — and when it was fetched, which tells keepFresh
// when the next one is due.
type guide struct {
	document []byte
	lineup   *[]channel
	made     time.Time
}

// xmltv serves the TV guide of the whole lineup in XMLTV format, from the
// cache keepFresh keeps filled: gathering a guide upstream can take minutes,
// which Plex is not made to wait. Only a guide that is not at hand — before
// the first fetch has finished, or after a Scan it has yet to follow — is
// fetched on the spot.
func (t *Tuner) xmltv(w http.ResponseWriter, r *http.Request) {
	document, err := t.guideDocument(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return // the viewer left: there is nobody to answer
		}
		// Better no answer than half a guide: Plex keeps the one it has.
		slog.Error("guide failed", "provider", t.provider.Name(), "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write(document)
}

// guideDocument returns the guide of the current lineup, fetching it first
// when there is none. However old, the guide there is beats fetching anew
// with Plex waiting: keepFresh sees to its age.
func (t *Tuner) guideDocument(ctx context.Context) ([]byte, error) {
	if g := t.currentGuide(); g != nil {
		return g.document, nil
	}
	select {
	case t.fetching <- struct{}{}:
	case <-ctx.Done(): // a viewer that is gone waits for nothing
		return nil, ctx.Err()
	}
	defer func() { <-t.fetching }()
	if g := t.currentGuide(); g != nil {
		return g.document, nil // fetched while this request waited
	}
	// The fetch outlives a viewer that leaves while it runs: the cache wants
	// the answer whoever asked for it.
	return t.fetchGuide(context.WithoutCancel(ctx))
}

// currentGuide returns the cached guide of the current lineup, or nil when
// there is none, or the lineup has moved on and may number the channels anew.
func (t *Tuner) currentGuide() *guide {
	g := t.guide.Load()
	if g == nil || g.lineup != t.lineup.Load() {
		return nil
	}
	return g
}

// keepFresh keeps the guide fetched ahead of Plex asking for it: right away,
// so a tuner fresh from a restart has one at hand, after every Scan, whose
// lineup the guide follows, and on a schedule, so what is served stays ahead
// of the clock. It runs until ctx ends.
//
// A fetch that fails leaves the guide there was, which Plex is given rather
// than nothing, and is tried again sooner: see guideRetry.
func (t *Tuner) keepFresh(ctx context.Context) {
	retry := guideRetry
	for {
		wait, err := t.refreshGuide(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.Error("guide refresh failed", "provider", t.provider.Name(), "err", err)
			// A source that stays down is asked less and less often.
			wait, retry = retry, min(2*retry, guideRefresh)
		default:
			retry = guideRetry // a fetch succeeded, whosever it was
		}
		select {
		case <-ctx.Done():
			return
		case <-t.nudge: // a Scan has published a new lineup
		case <-time.After(wait):
		}
	}
}

// refreshGuide fetches the guide, unless one fresh enough is at hand: fetched
// meanwhile, by a request or an earlier pass. It returns how long the guide
// stays fresh, which is when the next fetch is due.
func (t *Tuner) refreshGuide(ctx context.Context) (time.Duration, error) {
	select {
	case t.fetching <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-t.fetching }()
	if g := t.currentGuide(); g != nil {
		if age := time.Since(g.made); age < guideRefresh {
			return guideRefresh - age, nil
		}
	}
	if _, err := t.fetchGuide(ctx); err != nil {
		return 0, err
	}
	return guideRefresh, nil
}

// fetchGuide asks the provider for the programmes of the whole lineup,
// renders the XMLTV document and caches it. The caller holds the t.fetching
// token.
//
// Channels are identified by their lineup number, so the guide and the lineup
// always agree on which channel is which.
func (t *Tuner) fetchGuide(ctx context.Context) ([]byte, error) {
	began := time.Now()
	lineup := t.lineup.Load()
	tv := xmlTV{Generator: "telesfor"}
	var channels []provider.Channel
	numbers := map[string]string{} // the provider's channel id → lineup number
	if lineup != nil {
		for _, ch := range *lineup {
			c := xmlChannel{ID: ch.number, Name: ch.Name}
			if ch.Logo != "" {
				c.Icon = &xmlIcon{ch.Logo}
			}
			tv.Channels = append(tv.Channels, c)
			channels = append(channels, ch.Channel)
			numbers[ch.ID] = ch.number
		}
	}

	if len(channels) > 0 {
		from := time.Now().Truncate(time.Hour)
		programmes, err := t.provider.Programmes(ctx, channels, from, from.Add(guideSpan))
		if err != nil {
			return nil, err
		}
		for _, programme := range programmes {
			number, ok := numbers[programme.ChannelID]
			if !ok {
				continue
			}
			p := xmlProgramme{
				Start:   programme.Start.Format(xmltvTime),
				Stop:    programme.Stop.Format(xmltvTime),
				Channel: number,
				Title:   programme.Title,
				Desc:    programme.Description,
			}
			if programme.Image != "" {
				p.Icon = &xmlIcon{programme.Image}
			}
			tv.Programmes = append(tv.Programmes, p)
		}
	}

	var document bytes.Buffer
	document.WriteString(xml.Header)
	encoder := xml.NewEncoder(&document)
	encoder.Indent("", "  ")
	if err := encoder.Encode(tv); err != nil {
		return nil, err
	}
	t.guide.Store(&guide{document: document.Bytes(), lineup: lineup, made: time.Now()})
	slog.Debug("guide fetched", "provider", t.provider.Name(), "programmes", len(tv.Programmes), "took", since(began))
	return document.Bytes(), nil
}
