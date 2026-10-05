// Package ebc provides the live channels of EBC, Brazil's public broadcaster:
// TV Brasil, TV Brasil Internacional, Canal Gov and Canal Educação. Their
// streams are open to all, at addresses that stay the same, and TV Brasil's
// site has its guide.
package ebc

import (
	"context"
	"fmt"
	"html"
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
	// television is TV Brasil's broadcast among the guides of its site, which
	// keeps a day to a page: /programacao/20261005/2. The site has no guide of
	// the web stream, which leaves out what EBC has no rights to show online.
	television = "2"

	// guideDay is the format of a day in the guide's address.
	guideDay = "20060102"

	// block is how long a placeholder lasts in the guide.
	block = time.Hour

	// What a placeholder says of itself.
	unlisted = "EBC has published no listings for this time."
	unnamed  = "EBC's guide has no name for this programme."
)

// brt is the time of Brasília, which the guide is laid out in without saying
// so: the site's mark of what is on now agrees. Brazil has no summer time.
var brt = time.FixedZone("BRT", -3*60*60)

// channel is a channel as EBC streams it.
type channel struct {
	id, name, logo string
	stream         string // its master playlist
	guide          string // the days of its guide; empty if EBC publishes none
}

// channels are the channels to offer. A channel's place here is its place in
// the lineup for good: add new ones at the end.
//
// Canal Libras is not among them: the stream its page names answers 404.
var channels = []channel{
	{
		id:     "tv-brasil",
		name:   "TV Brasil",
		logo:   "https://tvbrasil.ebc.com.br/sites/default/themes/tvbrasil/img/logo_tvbrasil.png",
		stream: "https://tvbrasil-stream.ebc.com.br/index.m3u8",
		guide:  "https://tvbrasil.ebc.com.br/programacao",
	},
	{
		id:     "tv-brasil-internacional",
		name:   "TV Brasil Internacional",
		logo:   "https://tvbrasilinternacional.ebc.com.br/++plone++ebc.tvbrasilinternacional/android-chrome-512x512.png",
		stream: "https://tvbrasilinternacional-stream.ebc.com.br/index.m3u8",
	},
	{
		id:     "canal-gov",
		name:   "Canal Gov",
		logo:   "https://canalgov.ebc.com.br/++plone++ebc.canalgov/android-chrome-512x512.png",
		stream: "https://canalgov-stream.ebc.com.br/index.m3u8",
	},
	{
		id:     "canal-educacao",
		name:   "Canal Educação", // its site has a logo in SVG only, which Plex does not draw
		stream: "https://canaleducacao-stream.ebc.com.br/index.m3u8",
	},
}

// Provider streams EBC's live channels.
type Provider struct {
	client   *http.Client
	channels []channel // tests point them at a fake
}

// New returns an EBC provider.
//
// The streams play anywhere, but EBC keeps some of its pages to Brazil. Should
// it do so with the streams or the guide, a proxy gets past it: when proxy is
// set, all traffic goes through it.
func New(proxy string) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("ebc: %w", err)
	}
	return through(client, channels), nil
}

// through returns a provider that reaches the channels with client.
func through(client *http.Client, channels []channel) *Provider {
	client.Transport = oneQuality{client.Transport, channels}
	return &Provider{client: client, channels: channels}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "ebc" }

// Channels lists the channels, which are always the same.
func (p *Provider) Channels(context.Context) ([]provider.Channel, error) {
	listed := make([]provider.Channel, len(p.channels))
	for i, ch := range p.channels {
		listed[i] = provider.Channel{ID: ch.id, Name: ch.name, Logo: ch.logo, Place: i + 1}
	}
	return listed, nil
}

// Programmes fetches the guide of the channels that have one, and fills in
// for the rest: Plex offers a channel by what is on it, so a channel with
// nothing on has nothing to pick there.
func (p *Provider) Programmes(ctx context.Context, channels []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	var programmes []provider.Programme
	for _, listed := range channels {
		var known []provider.Programme
		if ch, ok := p.find(listed.ID); ok && ch.guide != "" {
			var err error
			if known, err = p.listings(ctx, ch, from, to); err != nil {
				return nil, fmt.Errorf("ebc: fetching guide: %w", err)
			}
			// As when the site lays its guide out anew. Placeholders alone
			// would wipe what Plex has of the channel.
			if len(known) == 0 {
				return nil, fmt.Errorf("ebc: fetching guide: nothing on %s", ch.name)
			}
		}
		programmes = append(programmes, fill(listed, known, from, to)...)
	}
	return programmes, nil
}

// row is a line of a day's listing: the time a programme starts at, then its
// name, mostly as a link to its page.
var (
	row = regexp.MustCompile(`(?s)class="date-display-single">(\d\d:\d\d)</span>\s*</div>\s*<div[^>]*nomeprograma">(.*?)</div>`)
	tag = regexp.MustCompile(`<[^>]*>`)
)

// listings returns what a channel's guide has between from and to.
//
// The guide gives no more of a programme than its start and, mostly, its name.
// So a programme runs until the next one starts, which for the last of a day
// is the first of the next. That takes the days around those asked for.
func (p *Provider) listings(ctx context.Context, ch channel, from, to time.Time) ([]provider.Programme, error) {
	type slot struct {
		start time.Time
		title string
	}
	var slots []slot
	year, month, day := from.In(brt).Date()
	first, last := time.Date(year, month, day-1, 0, 0, 0, 0, brt), to.AddDate(0, 0, 1)
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		page, _, status, err := p.get(ctx, ch.guide+"/"+day.Format(guideDay)+"/"+television)
		switch {
		case err != nil:
			return nil, err
		case status == http.StatusNotFound: // a day EBC has yet to publish: the guide reaches a week ahead
			continue
		case status != http.StatusOK:
			return nil, refusal(ch.name+"'s guide", status)
		}
		for _, line := range row.FindAllStringSubmatch(page, -1) {
			clock, err := time.Parse("15:04", line[1])
			if err != nil {
				continue
			}
			slots = append(slots, slot{
				start: day.Add(time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute),
				title: strings.TrimSpace(html.UnescapeString(tag.ReplaceAllString(line[2], ""))),
			})
		}
	}

	var programmes []provider.Programme
	for i := 0; i < len(slots)-1; i++ {
		start, stop := slots[i].start, slots[i+1].start
		// A day or more until the next programme is a day the guide lacks:
		// when this one ends is not known.
		if !stop.After(start) || stop.Sub(start) >= 24*time.Hour || !stop.After(from) || !start.Before(to) {
			continue
		}
		programme := provider.Programme{ChannelID: ch.id, Title: slots[i].title, Start: start, Stop: stop}
		if programme.Title == "" {
			programme.Title, programme.Description = ch.name, unnamed
		}
		programmes = append(programmes, programme)
	}
	return programmes, nil
}

// fill returns the programmes of a channel with a placeholder wherever there
// is none between from and to: the channel's name, an hour at a time by the
// clock.
func fill(ch provider.Channel, known []provider.Programme, from, to time.Time) []provider.Programme {
	var programmes []provider.Programme
	at := from
	until := func(next time.Time) {
		for at.Before(next) {
			stop := at.Truncate(block).Add(block)
			if stop.After(next) {
				stop = next
			}
			programmes = append(programmes, provider.Programme{
				ChannelID: ch.ID, Title: ch.Name, Description: unlisted, Start: at, Stop: stop,
			})
			at = stop
		}
	}
	for _, programme := range known {
		until(programme.Start)
		programmes = append(programmes, programme)
		if programme.Stop.After(at) {
			at = programme.Stop
		}
	}
	until(to)
	return programmes
}

// Stream returns the channel's master playlist, once a look at it finds the
// channel playing. A channel that does not play is told apart by what would
// help: see refusal.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	ch, ok := p.find(channelID)
	if !ok {
		return provider.Source{}, fmt.Errorf("ebc: no channel %q", channelID)
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
			return provider.Source{}, fmt.Errorf("ebc: %s is unavailable: %w", ch.name, err)
		}
		if media, _, err = p.playlist(ctx, ch, address.String()); err != nil {
			return provider.Source{}, err
		}
	}
	switch {
	// AES-128 is no obstacle: its key is there for ffmpeg to fetch.
	case strings.Contains(master+media, "METHOD=SAMPLE-AES"):
		return provider.Source{}, fmt.Errorf("ebc: %s is DRM-protected", ch.name)
	case !strings.Contains(media, "#EXTINF"):
		return provider.Source{}, fmt.Errorf("ebc: %s is unavailable: its playlist is empty", ch.name)
	}
	// The master, not the quality: the sound is in a playlist of its own.
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

// oneQuality is the transport of the provider's HTTP client. It passes on
// what it fetches as it is, but for a channel's master playlist, which it cuts
// down to the best quality.
//
// Given them all, ffmpeg reads the start of every quality before it settles
// on the best. EBC's CDN takes seconds over a segment that nobody nearby has
// asked for yet, which far from Brazil is most of them: a channel took up to
// fifteen seconds to start.
type oneQuality struct {
	http.RoundTripper
	channels []channel
}

func (t oneQuality) RoundTrip(req *http.Request) (*http.Response, error) {
	if !slices.ContainsFunc(t.channels, func(ch channel) bool { return ch.stream == req.URL.String() }) {
		return t.RoundTripper.RoundTrip(req)
	}
	// The whole of it, even for ffmpeg, which asks for a range of everything:
	// a part of a playlist is nothing to cut down.
	whole := req.Clone(req.Context())
	whole.Header.Del("Range")
	resp, err := t.RoundTripper.RoundTrip(whole)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	playlist := best(string(body))
	resp.Body = io.NopCloser(strings.NewReader(playlist))
	resp.ContentLength = int64(len(playlist))
	resp.Header.Set("Content-Length", strconv.Itoa(len(playlist)))
	return resp, nil
}

// best returns a master playlist without its qualities other than the highest.
// What they share, such as the sound, stays.
func best(master string) string {
	lines := strings.Split(master, "\n")
	keep, most := -1, -1 // the line that announces the highest quality, and its bandwidth
	for i, line := range lines {
		attributes, ok := strings.CutPrefix(line, "#EXT-X-STREAM-INF:")
		if !ok {
			continue
		}
		for attribute := range strings.SplitSeq(attributes, ",") {
			if bandwidth, ok := strings.CutPrefix(attribute, "BANDWIDTH="); ok {
				if n, _ := strconv.Atoi(bandwidth); n > most {
					keep, most = i, n
				}
			}
		}
	}
	var kept []string
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "#EXT-X-STREAM-INF:") && i != keep {
			i++ // its URI is the line after
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.Join(kept, "\n")
}

// playlist fetches an HLS playlist of a channel, and returns it with the
// address it came from.
func (p *Provider) playlist(ctx context.Context, ch channel, address string) (string, *url.URL, error) {
	page, at, status, err := p.get(ctx, address)
	switch {
	case err != nil:
		return "", nil, fmt.Errorf("ebc: reaching %s: %w", ch.name, err)
	case status != http.StatusOK:
		return "", nil, fmt.Errorf("ebc: %w", refusal(ch.name, status))
	case !strings.HasPrefix(page, "#EXTM3U"):
		return "", nil, fmt.Errorf("ebc: %s is unavailable: EBC sends no playlist", ch.name)
	}
	return page, at, nil
}

// refusal puts into words that EBC did not hand something out, by what would
// help. Its streams take no account and play abroad, so 401 and 403 are EBC
// changing that. Anything else is a stream or a guide that is not there, as
// when EBC takes a channel off the web, and neither an account nor a proxy
// brings it back.
func refusal(what string, status int) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s asks for a sign-in, which telesfor has none for", what)
	case http.StatusForbidden, http.StatusUnavailableForLegalReasons:
		return fmt.Errorf("%s is refused here, as EBC does outside Brazil: set -ebc-proxy to a proxy with a Brazilian exit, or try another exit", what)
	}
	return fmt.Errorf("%s is unavailable: EBC answers %d %s", what, status, http.StatusText(status))
}

// get fetches a page of EBC's: a playlist, or a day of the guide. It returns
// the page with the address it came from, after redirects, and its status.
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
	status = resp.StatusCode
	// EBC's sites turn an address abroad away by sending it on to a page that
	// says so, which itself answers 200.
	if strings.HasSuffix(resp.Request.URL.Path, "/403.html") {
		status = http.StatusForbidden
	}
	return string(body), resp.Request.URL, status, nil
}
