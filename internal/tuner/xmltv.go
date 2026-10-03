package tuner

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// guideSpan is how far ahead the guide reaches.
const guideSpan = 48 * time.Hour

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
		Start   string `xml:"start,attr"`
		Stop    string `xml:"stop,attr"`
		Channel string `xml:"channel,attr"`
		Title   string `xml:"title"`
		Desc    string `xml:"desc,omitempty"`
	}
)

// xmltv serves the TV guide of the whole lineup in XMLTV format.
//
// Channels are identified by their lineup number, so the guide and the lineup
// always agree on which channel is which.
func (t *Tuner) xmltv(w http.ResponseWriter, r *http.Request) {
	guide := xmlTV{Generator: "telesfor"}
	for _, ch := range t.lineup {
		c := xmlChannel{ID: ch.number, Name: ch.Name}
		if ch.Logo != "" {
			c.Icon = &xmlIcon{ch.Logo}
		}
		guide.Channels = append(guide.Channels, c)
	}

	from := time.Now().Truncate(time.Hour)
	for _, p := range t.providers {
		var channels []provider.Channel
		numbers := map[string]string{} // the provider's channel id → lineup number
		for _, ch := range t.lineup {
			if ch.provider.Name() == p.Name() {
				channels = append(channels, ch.Channel)
				numbers[ch.ID] = ch.number
			}
		}

		programmes, err := p.Programmes(r.Context(), channels, from, from.Add(guideSpan))
		if err != nil {
			// Better no answer than half a guide: Plex keeps the one it has.
			slog.Error("guide failed", "provider", p.Name(), "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for _, programme := range programmes {
			number, ok := numbers[programme.ChannelID]
			if !ok {
				continue
			}
			guide.Programmes = append(guide.Programmes, xmlProgramme{
				Start:   programme.Start.Format(xmltvTime),
				Stop:    programme.Stop.Format(xmltvTime),
				Channel: number,
				Title:   programme.Title,
				Desc:    programme.Description,
			})
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	io.WriteString(w, xml.Header)
	encoder := xml.NewEncoder(w)
	encoder.Indent("", "  ")
	encoder.Encode(guide)
}
