// Package cultura provides the live channels of TV Cultura, São Paulo's public
// broadcaster: TV Cultura itself and Cultura Fast, its streaming channel of
// current and archive programmes. Their streams are open to all, at addresses
// that stay the same, and TV Cultura's site has its guide.
package cultura

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

const (
	// guideDay is the format of a day in the guide's address, which keeps a
	// day to a page: /grade/05102026.html.
	guideDay = "02012006"

	// unlisted is what a placeholder says of itself.
	unlisted = "TV Cultura has no reliable listings for this time."
)

// brt is the time of Brasília, which the guide is laid out in without saying
// so: the site's mark of what is on now agrees. Brazil has no summer time.
var brt = time.FixedZone("BRT", -3*60*60)

// channel is a channel as TV Cultura streams it.
type channel struct {
	id, name, logo string
	stream         string // its master playlist
	guide          string // the days of its guide; empty if it has none to go by
}

// channels are the channels to offer. A channel's place here is its place in
// the lineup for good: add new ones at the end.
//
// The broadcaster has more to its name, but no more television to stream:
// its radio stations, TV Rá-Tim-Bum, which is sold by subscription, and
// Univesp TV, which it shows through YouTube.
var channels = []channel{
	{
		id:     "tv-cultura",
		name:   "TV Cultura",
		logo:   "https://cdn.aws.eitvcloud.com/account351-i1438/channel/14/logo-rosa-novo-1755800911.png",
		stream: "https://player-tvcultura.stream.uol.com.br/live/tvcultura.m3u8",
		guide:  "https://cultura.uol.com.br/grade",
	},
	{
		// Cultura Play, the broadcaster's streaming site, lists programmes for
		// it, but the stream does not keep to them: watched for an hour in
		// October 2026, it showed none of the four that the site named.
		id:     "cultura-fast",
		name:   "Cultura Fast",
		logo:   "https://cdn.aws.eitvcloud.com/account351-i1438/channel/53/quadrada_-3-_pequeno-1723832301.png",
		stream: "https://fpa-gateway.tvcultura.com.br:8181/memfs/606caef0-a290-413d-9f1f-8fcdb3a73831.m3u8",
	},
}

// Provider streams TV Cultura's live channels.
type Provider struct {
	client   *http.Client
	channels []channel // tests point them at a fake
}

// New returns a TV Cultura provider.
//
// The streams and the guide are open to any address. Should TV Cultura keep
// them to Brazil, a proxy gets past it: when proxy is set, all traffic goes
// through it.
func New(proxy string) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("cultura: %w", err)
	}
	return &Provider{client: client, channels: channels}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "cultura" }

// Channels lists the channels, which are always the same.
func (p *Provider) Channels(context.Context) ([]provider.Channel, error) {
	listed := make([]provider.Channel, len(p.channels))
	for i, ch := range p.channels {
		listed[i] = provider.Channel{ID: ch.id, Name: ch.name, Logo: ch.logo, Place: i + 1}
	}
	return listed, nil
}

// Programmes fetches the guide of the channel that has one, and fills in for
// the rest: Plex offers a channel by what is on it, so a channel with nothing
// on has nothing to pick there.
func (p *Provider) Programmes(ctx context.Context, channels []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	var programmes []provider.Programme
	for _, listed := range channels {
		var known []provider.Programme
		if ch, ok := p.find(listed.ID); ok && ch.guide != "" {
			var err error
			if known, err = p.listings(ctx, ch, from, to); err != nil {
				return nil, fmt.Errorf("cultura: fetching guide: %w", err)
			}
			// As when the site lays its guide out anew. Placeholders alone
			// would wipe what Plex has of the channel.
			if len(known) == 0 {
				return nil, fmt.Errorf("cultura: fetching guide: nothing on %s", ch.name)
			}
		}
		programmes = append(programmes, provider.Fill(listed, known, from, to, unlisted)...)
	}
	return programmes, nil
}

// listings returns what a channel's guide has between from and to.
//
// The guide gives a programme's start, but not its end. So a programme runs
// until the next one starts, which for the last of a day is the first of the
// next. The day before those asked for is read as well: a day of the guide
// runs into the next morning.
func (p *Provider) listings(ctx context.Context, ch channel, from, to time.Time) ([]provider.Programme, error) {
	var listed []provider.Programme
	year, month, day := from.In(brt).Date()
	for day := time.Date(year, month, day-1, 0, 0, 0, 0, brt); day.Before(to); day = day.AddDate(0, 0, 1) {
		page, _, status, err := p.get(ctx, ch.guide+"/"+day.Format(guideDay)+".html")
		switch {
		case err != nil:
			return nil, err
		case status == http.StatusNotFound: // a day TV Cultura has yet to publish: the guide reaches two days ahead
			continue
		case status != http.StatusOK:
			return nil, refusal(ch.name+"'s guide", status)
		}
		listed = append(listed, broadcast(page, day)...)
	}
	return provider.UntilNext(ch.id, listed, from, to), nil
}

// What a day of the guide has of a programme: the time it starts at, its
// name, a picture, and under "more" the episode's name and what it is about.
// The last two are often left out.
var (
	clock   = regexp.MustCompile(`<time>(\d\d):(\d\d)</time>`)
	name    = regexp.MustCompile(`(?s)<h3>(.*?)</h3>`)
	picture = regexp.MustCompile(`<img src="([^"]*)"`)
	more    = regexp.MustCompile(`(?s)<section class="mais">\s*<section>\s*(?:<h2>(.*?)</h2>)?\s*<div>(.*?)</div>`)
)

// broadcast reads a day of the guide, which is a day of the broadcast: it
// starts at about six in the morning and runs into the next, so the times
// that turn back to the small hours are of the day after. The programmes it
// returns have no end yet.
func broadcast(page string, day time.Time) []provider.Programme {
	var programmes []provider.Programme
	for _, entry := range strings.Split(page, `<section class="grade-box`)[1:] {
		at := clock.FindStringSubmatch(entry)
		if at == nil {
			continue
		}
		hours, _ := strconv.Atoi(at[1])
		minutes, _ := strconv.Atoi(at[2])
		programme := provider.Programme{Start: day.Add(time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute)}
		if last := len(programmes) - 1; last >= 0 && programme.Start.Before(programmes[last].Start) {
			programme.Start = programme.Start.AddDate(0, 0, 1)
		}
		if title := name.FindStringSubmatch(entry); title != nil {
			programme.Title = provider.PlainText(title[1])
		}
		if about := more.FindStringSubmatch(entry); about != nil {
			programme.Description = strings.TrimSpace(provider.PlainText(about[1]) + "\n" + provider.PlainText(about[2]))
		}
		// A programme without a picture has one of the site's own in its
		// place, at an address within the site.
		if src := picture.FindStringSubmatch(entry); src != nil && strings.HasPrefix(src[1], "https://") {
			programme.Image = src[1]
		}
		programmes = append(programmes, programme)
	}
	return programmes
}

// Stream returns the channel's master playlist, once a look at it finds the
// channel playing. A channel that does not play is told apart by what would
// help: see refusal.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	ch, ok := p.find(channelID)
	if !ok {
		return provider.Source{}, fmt.Errorf("cultura: no channel %q", channelID)
	}
	master, at, err := p.playlist(ctx, ch, ch.stream)
	if err != nil {
		return provider.Source{}, err
	}
	// Whether the stream is encrypted, or has stopped, shows in the playlist
	// of its one quality.
	media := master
	if uri := quality(master); uri != "" {
		address, err := at.Parse(uri)
		if err != nil {
			return provider.Source{}, fmt.Errorf("cultura: %s is unavailable: %w", ch.name, err)
		}
		if media, _, err = p.playlist(ctx, ch, address.String()); err != nil {
			return provider.Source{}, err
		}
	}
	switch {
	// AES-128 is no obstacle: its key is there for ffmpeg to fetch.
	case strings.Contains(master+media, "METHOD=SAMPLE-AES"):
		return provider.Source{}, fmt.Errorf("cultura: %s is DRM-protected", ch.name)
	case !strings.Contains(media, "#EXTINF"):
		return provider.Source{}, fmt.Errorf("cultura: %s is unavailable: its playlist is empty", ch.name)
	}
	// The master, not the quality: Cultura Fast's is at an address made for
	// whoever asks, and ffmpeg is to ask for its own.
	return provider.Source{URL: ch.stream, Client: p.client}, nil
}

// find looks a channel up by its id.
func (p *Provider) find(id string) (channel, bool) {
	i := slices.IndexFunc(p.channels, func(ch channel) bool { return ch.id == id })
	if i < 0 {
		return channel{}, false
	}
	return p.channels[i], true
}

// quality returns the URI of the first quality in a master playlist, which is
// the only one in a channel's, or "" for a playlist that is not one.
func quality(master string) string {
	listed := false // the line before announced a quality
	for line := range strings.Lines(master) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			listed = true
		case listed && line != "" && !strings.HasPrefix(line, "#"):
			return line
		}
	}
	return ""
}

// playlist fetches an HLS playlist of a channel, and returns it with the
// address it came from.
func (p *Provider) playlist(ctx context.Context, ch channel, address string) (string, *url.URL, error) {
	page, at, status, err := p.get(ctx, address)
	switch {
	case err != nil:
		return "", nil, fmt.Errorf("cultura: reaching %s: %w", ch.name, err)
	case status != http.StatusOK:
		return "", nil, fmt.Errorf("cultura: %w", refusal(ch.name, status))
	case !strings.HasPrefix(page, "#EXTM3U"):
		return "", nil, fmt.Errorf("cultura: %s is unavailable: TV Cultura sends no playlist", ch.name)
	}
	return page, at, nil
}

// refusal puts into words that TV Cultura did not hand something out, by what
// would help. Its streams take no account and play abroad, so 401 and 403 are
// TV Cultura changing that. Anything else is a stream or a guide that is not
// there, as when TV Cultura takes a channel off the web, and neither an
// account nor a proxy brings it back.
func refusal(what string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s asks for a sign-in, which telesfor has none for", what)
	case http.StatusForbidden, http.StatusUnavailableForLegalReasons:
		return fmt.Errorf("%s is refused here, as it may be outside Brazil: set -cultura-proxy to a proxy with a Brazilian exit, or try another exit", what)
	}
	return fmt.Errorf("%s is unavailable: TV Cultura answers %d %s", what, status, http.StatusText(status))
}

// get fetches a page of TV Cultura's: a playlist, or a day of the guide. It
// returns the page with the address it came from, after redirects, and its
// status.
func (p *Provider) get(ctx context.Context, address string) (page string, at *url.URL, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", nil, 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, 0, err
	}
	return string(body), resp.Request.URL, resp.StatusCode, nil
}
