// Package tvp provides the live channels of TVP, the Polish public broadcaster,
// using the API behind vod.tvp.pl.
package tvp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

const (
	// apiURL is the API the vod.tvp.pl website itself talks to. It needs no login.
	apiURL = "https://vod.tvp.pl/api/products"

	// guideWindow is the longest span of guide the API returns in one request.
	guideWindow = 24 * time.Hour

	// guideTime is the format of the guide's since and till parameters.
	guideTime = "2006-01-02T15:04-0700"

	// patience is how long a tune keeps asking for a stream that is refused
	// where TVP hands it out, and pause is how long it waits between asking:
	// see serving.
	patience = 15 * time.Second
	pause    = 2 * time.Second

	// search is how long a provider that has listed its channels goes on
	// asking where a stream is served, and interval is how long it waits
	// between asking: see learn.
	search   = 5 * time.Minute
	interval = 10 * time.Second
)

// errRefused is a host turning this address away from a stream.
var errRefused = errors.New("refused here")

// images are the pictures the API attaches to a channel or a programme, keyed
// by aspect ratio, such as "3x4".
type images map[string][]struct{ URL string }

// url returns the address of the picture of the given aspect ratio, or "" when
// there is none.
func (i images) url(ratio string) string {
	if len(i[ratio]) == 0 {
		return ""
	}
	return "https:" + i[ratio][0].URL // the API leaves the scheme out: //s.tvp.pl/…
}

// Provider streams TVP's live channels.
type Provider struct {
	api    string
	client *http.Client

	patience, pause  time.Duration // see the constants; tests are in more of a hurry
	search, interval time.Duration

	mu       sync.Mutex
	served   string // the host that last served a stream, as https://host: see serving
	learning bool   // whether learn is asking for one
}

// New returns a TVP provider.
//
// TVP blocks most of its channels outside Poland and binds every stream to the
// address that asked for it. So when proxy is set, all traffic goes through it:
// the API calls made here and, later, the stream itself.
func New(proxy string) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("tvp: %w", err)
	}
	return &Provider{api: apiURL, client: client, patience: patience, pause: pause, search: search, interval: interval}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "tvp" }

// Channels lists the channels that are free to watch.
func (p *Provider) Channels(ctx context.Context) ([]provider.Channel, error) {
	var lives struct {
		Items []struct {
			ID            int
			Title         string
			Payable       bool
			LoginRequired bool
			LiveType      string
			LogoImages    images
		}
	}
	if err := p.get(ctx, "/lives", url.Values{"lang": {"PL"}}, &lives); err != nil {
		return nil, fmt.Errorf("tvp: listing channels: %w", err)
	}

	var channels []provider.Channel
	for _, live := range lives.Items {
		// Subscription channels need an account, and FAST channels are encrypted.
		if live.Payable || live.LoginRequired || live.LiveType == "FAST" {
			continue
		}
		channels = append(channels, provider.Channel{
			ID:   strconv.Itoa(live.ID),
			Name: live.Title,
			Logo: live.LogoImages.url("1x1"),
		})
	}
	if len(channels) > 0 {
		p.learn(channels[0].ID)
	}
	return channels, nil
}

// Programmes fetches the guide, one request per guideWindow.
func (p *Provider) Programmes(ctx context.Context, channels []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	query := url.Values{"lang": {"PL"}}
	for _, channel := range channels {
		query.Add("liveId[]", channel.ID)
	}

	var programmes []provider.Programme
	seen := map[int]bool{} // a programme that spans two windows comes back in both
	for since := from; since.Before(to); since = since.Add(guideWindow) {
		till := since.Add(guideWindow)
		if till.After(to) {
			till = to
		}
		query.Set("since", since.Format(guideTime))
		query.Set("till", till.Format(guideTime))

		var items []struct {
			ID          int
			Title       string
			Description string
			Lead        string // a shorter description
			Images      images
			Since, Till time.Time
			Live        struct{ ID int }
		}
		if err := p.get(ctx, "/lives/programmes", query, &items); err != nil {
			return nil, fmt.Errorf("tvp: fetching guide: %w", err)
		}
		for _, item := range items {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			programmes = append(programmes, provider.Programme{
				ChannelID:   strconv.Itoa(item.Live.ID),
				Title:       item.Title,
				Description: cmp.Or(item.Description, item.Lead),
				Image:       item.Images.url("3x4"), // upright, like the posters Plex shows
				Start:       item.Since,
				Stop:        item.Till,
			})
		}
	}
	return programmes, nil
}

// Stream asks TVP for a fresh HLS URL of the channel. The URL carries a token
// that is only valid from the address that made this request.
//
// TVP may hand the stream out at a host that refuses this address. It names
// another host from time to time, so Stream asks again for a while: see
// serving.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	for began := time.Now(); ; {
		stream, err := p.handedOut(ctx, channelID)
		if err != nil {
			return provider.Source{}, fmt.Errorf("tvp: %w", err)
		}
		if stream, err = p.serving(ctx, stream); err == nil {
			return provider.Source{URL: stream, Client: p.client}, nil
		}
		if !errors.Is(err, errRefused) || time.Since(began) >= p.patience {
			return provider.Source{}, fmt.Errorf("tvp: %w", err)
		}
		select {
		case <-ctx.Done():
			return provider.Source{}, fmt.Errorf("tvp: %w", ctx.Err())
		case <-time.After(p.pause):
		}
	}
}

// handedOut asks the API where the channel's stream is.
func (p *Provider) handedOut(ctx context.Context, channelID string) (string, error) {
	var playlist struct {
		Sources struct {
			HLS []struct{ Src string }
		}
		DRM map[string]json.RawMessage // license servers; present only on encrypted channels
	}
	path := "/" + url.PathEscape(channelID) + "/videos/playlist"
	if err := p.get(ctx, path, url.Values{"videoType": {"LIVE"}}, &playlist); err != nil {
		return "", fmt.Errorf("resolving stream: %w", err)
	}
	if len(playlist.DRM) > 0 {
		return "", errors.New("channel is DRM-protected")
	}
	if len(playlist.Sources.HLS) == 0 {
		return "", errors.New("channel has no HLS stream")
	}
	return playlist.Sources.HLS[0].Src, nil
}

// serving returns where to play a stream from: where TVP handed it out, or
// the same stream at another of TVP's hosts.
//
// TVP hands a stream out at one of its own servers or at a router that sends
// on to a CDN, as it sees fit, the same one for half a minute at a time. The
// router answers the addresses of VPN providers with 403, where TVP's own
// servers serve them. And what makes a stream out, its path with the token in
// it, is good at either. So a stream that is refused where it was handed out
// is asked for at the host that last served one.
func (p *Provider) serving(ctx context.Context, stream string) (string, error) {
	handed, err := url.Parse(stream)
	if err != nil {
		return "", errors.New("the stream's address cannot be read")
	}
	host := handed.Scheme + "://" + handed.Host
	p.mu.Lock()
	served := p.served
	p.mu.Unlock()

	status, err := p.look(ctx, stream)
	switch {
	case err != nil:
		return "", fmt.Errorf("reaching the stream: %w", err)
	case status == http.StatusOK:
		p.remember(host)
		return stream, nil
	case status != http.StatusForbidden && status != http.StatusUnauthorized:
		return "", fmt.Errorf("the stream is unavailable: %s answers %d %s", handed.Host, status, http.StatusText(status))
	}
	if served != "" && served != host {
		elsewhere := served + handed.RequestURI()
		if status, err := p.look(ctx, elsewhere); err == nil && status == http.StatusOK {
			return elsewhere, nil
		}
		if ctx.Err() == nil { // a viewer who left says nothing of the host
			p.forget(served) // it serves no more
		}
	}
	return "", fmt.Errorf("the stream is %w by %s, as TVP's CDN refuses VPNs: tune again in a moment", errRefused, handed.Host)
}

// learn finds a host that serves streams before anyone tunes, unless one is
// known. It asks in the background where a channel's stream is, time and
// again, until a host serves it: the one serving then plays a refused stream
// from. So the first tune need not wait for TVP to name a server of its own.
func (p *Provider) learn(channelID string) {
	p.mu.Lock()
	start := p.search > 0 && p.served == "" && !p.learning
	if start {
		p.learning = true
	}
	p.mu.Unlock()
	if !start {
		return
	}

	go func() {
		// The search outlasts the request that started it.
		ctx, cancel := context.WithTimeout(context.Background(), p.search)
		defer cancel()
		defer func() {
			p.mu.Lock()
			p.learning = false
			p.mu.Unlock()
		}()
		for {
			stream, err := p.handedOut(ctx, channelID)
			if err == nil {
				_, err = p.serving(ctx, stream)
			}
			if !errors.Is(err, errRefused) {
				return // a host serves it, or asking again will not change the answer
			}
			select {
			case <-ctx.Done():
				slog.Warn("tvp: TVP's CDN refuses this address, and TVP names no server of its own: its channels may not play")
				return
			case <-time.After(p.interval):
			}
		}
	}()
}

// remember notes the host that served a stream.
func (p *Provider) remember(host string) {
	p.mu.Lock()
	p.served = host
	p.mu.Unlock()
}

// forget drops the host that served a stream, unless another has served one
// since.
func (p *Provider) forget(host string) {
	p.mu.Lock()
	if p.served == host {
		p.served = ""
	}
	p.mu.Unlock()
}

// look fetches a stream's playlist and returns how its host answers.
func (p *Provider) look(ctx context.Context, stream string) (status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stream, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, errors.Unwrap(err) // without the URL, which carries the stream's token
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// get calls the API and decodes its JSON response into v.
func (p *Provider) get(ctx context.Context, path string, query url.Values, v any) error {
	query.Set("platform", "BROWSER") // the API refuses requests that name no platform

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.api+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var failure struct{ Code string }
		json.NewDecoder(resp.Body).Decode(&failure) // the code is a bonus; the status alone will do
		if failure.Code == "GEOIP_FILTER_FAILED" {
			return errors.New("blocked outside Poland: set -tvp-proxy to a proxy with a Polish exit")
		}
		return errors.New(strings.TrimSpace(resp.Status + " " + failure.Code))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
