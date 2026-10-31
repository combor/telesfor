package tuner

import (
	"bytes"
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/store"
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
// when the next one is due. A guide from the store has no time: it is due at
// once.
type guide struct {
	document []byte
	lineup   *[]channel
	made     time.Time
}

// guidesBucket is the store's bucket of the last guide each tuner fetched, by
// its provider's name. After a restart, it is the guide there is until one is
// fetched anew: see restoreGuide.
const guidesBucket = "guides"

// savedGuide is a guide as the store keeps it. A lineup in memory is told by
// its address, which a restart changes: the store has it in full.
type savedGuide struct {
	Document []byte
	Lineup   []savedChannel
	Until    time.Time // the end of what the guide was fetched for
}

type savedChannel struct {
	provider.Channel
	Number string
}

func savedLineup(lineup []channel) []savedChannel {
	saved := make([]savedChannel, len(lineup))
	for i, ch := range lineup {
		saved[i] = savedChannel{ch.Channel, ch.number}
	}
	return saved
}

// restoreGuide caches the guide the store has from before a restart, if it is
// of the lineup there is now and still reaches ahead: while the source is
// down, Plex is better given that than nothing. keepFresh fetches anew at
// once all the same, as it would without one: the guide may be the work of an
// older telesfor. The caller holds t.scanning, or is New.
func (t *Tuner) restoreGuide() {
	var saved savedGuide
	found, err := store.Get(t.db, guidesBucket, t.provider.Name(), &saved)
	if err != nil {
		slog.Warn("saved guide unreadable", "provider", t.provider.Name(), "err", err)
		return
	}
	lineup := t.lineup.Load()
	if !found || lineup == nil || !slices.Equal(saved.Lineup, savedLineup(*lineup)) || !time.Now().Before(saved.Until) {
		return
	}
	if t.publish(&guide{document: saved.Document, lineup: lineup}) {
		slog.Debug("guide restored", "provider", t.provider.Name(), "until", saved.Until)
	}
}

// publish caches g if it is of the lineup there is now, and reports whether it
// was. A fetch can outlast its lineup, through a Scan or a Disable, and its
// guide would take the place of the current lineup's, such as one restored
// when the tuner was enabled again: the check and the store go together.
func (t *Tuner) publish(g *guide) bool {
	t.storing.Lock()
	defer t.storing.Unlock()
	if g.lineup != t.lineup.Load() {
		return false
	}
	t.guide.Store(g)
	return true
}

// xmltv serves the TV guide of the whole lineup in XMLTV format, from the
// cache keepFresh keeps filled: gathering a guide upstream can take minutes,
// which Plex is not made to wait. Only a guide that is not at hand — before
// the first fetch has finished, with none saved from before a restart, or
// after a Scan it has yet to follow — is fetched on the spot.
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
	// the answer whoever asked for it. It ends when the tuner is disabled.
	on := t.on.Load()
	if on == nil {
		return nil, context.Canceled
	}
	return t.fetchGuide(on.ctx)
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
// so a tuner fresh from a restart has one of its own at hand, after every
// Scan, whose lineup the guide follows, and on a schedule, so what is served
// stays ahead of the clock. It runs until ctx ends, which is when the tuner is
// disabled.
//
// A fetch that fails leaves the guide there was, which Plex is given rather
// than nothing, and is tried again sooner: see guideRetry.
func (t *Tuner) keepFresh(ctx context.Context) {
	for retry := guideRetry; ; {
		select {
		case t.fetching <- struct{}{}:
		case <-ctx.Done():
			return
		}
		var wait time.Duration
		if g := t.currentGuide(); g != nil {
			wait = guideRefresh - time.Since(g.made) // fetched meanwhile, by a request or an earlier pass
		}
		var err error
		if wait <= 0 {
			wait = guideRefresh
			_, err = t.fetchGuide(ctx)
		}
		<-t.fetching
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

// fetchGuide asks the provider for the programmes of the whole lineup,
// renders the XMLTV document and caches it, unless the lineup has moved on
// meanwhile. The caller holds the t.fetching token.
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

	from := time.Now().Truncate(time.Hour)
	until := from.Add(guideSpan)
	if len(channels) > 0 {
		programmes, err := t.provider.Programmes(ctx, channels, from, until)
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
	slog.Debug("guide fetched", "provider", t.provider.Name(), "programmes", len(tv.Programmes), "took", since(began))
	if !t.publish(&guide{document: document.Bytes(), lineup: lineup, made: time.Now()}) {
		return document.Bytes(), nil // of a lineup that is gone
	}
	if lineup != nil {
		// For a restart while the source is down: see restoreGuide.
		saved := savedGuide{Document: document.Bytes(), Lineup: savedLineup(*lineup), Until: until}
		if err := store.Put(t.db, guidesBucket, t.provider.Name(), saved); err != nil {
			slog.Warn("guide not saved", "provider", t.provider.Name(), "err", err)
		}
	}
	return document.Bytes(), nil
}
