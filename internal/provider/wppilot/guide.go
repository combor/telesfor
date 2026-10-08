package wppilot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

const (
	// unlisted is what a placeholder says of itself.
	unlisted = "WP Pilot has published no listings for this time."

	// batch is how many channels WP's guide is asked about at once: as many
	// as its own player asks about.
	batch = 20
)

// Programmes fetches the guide, which WP publishes for the half day ahead and
// no further. The rest of what is asked for has the channel's name on every
// hour, and so has a channel WP publishes no guide of: Plex offers a channel
// by what is on it, so a channel with nothing on has nothing to pick there.
//
// It is also when telesfor looks after the account: whether WP still accepts
// the sign-in, whether its consents are accepted, and which channels it gets.
func (p *Provider) Programmes(ctx context.Context, listed []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	p.verify(ctx)
	p.relist(ctx)

	known := map[string][]provider.Programme{}
	for channels := range slices.Chunk(listed, batch) {
		ids := make([]string, len(channels))
		for i, ch := range channels {
			ids[i] = ch.ID
		}
		var guide []struct {
			Channel int `json:"channel_id"`
			Entries []struct {
				Start, End                time.Time
				Title, Description, Photo string
			}
		}
		// The guide is for anyone to read: it takes no session.
		query := url.Values{"channels": {strings.Join(ids, ",")}, "limit": {"100"}, "device_type": {"web"}}
		answer, err := p.call(ctx, nil, http.MethodGet, p.site+"/api/v2/epg?"+query.Encode(), nil, &guide)
		if err == nil && answer.status != http.StatusOK {
			err = errors.New(strings.TrimSpace(http.StatusText(answer.status) + " " + answer.refused))
		}
		if err != nil {
			return nil, fmt.Errorf("wppilot: fetching guide: %w", err)
		}
		for _, ch := range guide {
			id := strconv.Itoa(ch.Channel)
			for _, entry := range ch.Entries {
				if entry.Title == "" || !entry.End.After(entry.Start) || !entry.End.After(from) || !entry.Start.Before(to) {
					continue
				}
				known[id] = append(known[id], provider.Programme{
					ChannelID:   id,
					Title:       entry.Title,
					Description: entry.Description,
					Image:       entry.Photo,
					Start:       entry.Start,
					Stop:        entry.End,
				})
			}
		}
	}
	// A channel without a guide is no fault: WP has none of its fireplace. A
	// guide of placeholders alone would wipe what Plex has of every channel.
	if len(known) == 0 && len(listed) > 0 {
		return nil, errors.New("wppilot: fetching guide: nothing on any channel")
	}

	var programmes []provider.Programme
	for _, ch := range listed {
		slices.SortFunc(known[ch.ID], func(a, b provider.Programme) int { return a.Start.Compare(b.Start) })
		programmes = append(programmes, provider.Fill(ch, known[ch.ID], from, to, unlisted)...)
	}
	return programmes, nil
}
