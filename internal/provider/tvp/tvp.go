// Package tvp provides the live channels of TVP, the Polish public broadcaster,
// using the API behind vod.tvp.pl.
package tvp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
)

// Provider streams TVP's live channels.
type Provider struct {
	api    string
	client *http.Client
}

// New returns a TVP provider.
//
// TVP blocks most of its channels outside Poland and binds every stream to the
// address that asked for it. So when proxy is set, all traffic goes through it:
// the API calls made here and, later, the stream itself. Without one, the
// proxy settings of the environment apply.
func New(proxy string) (*Provider, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil || proxyURL.Host == "" {
			return nil, fmt.Errorf("tvp: invalid proxy %q: want a URL like http://host:port", proxy)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &Provider{
		api:    apiURL,
		client: &http.Client{Transport: transport, Timeout: time.Minute},
	}, nil
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
			LogoImages    map[string][]struct{ URL string }
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
		channel := provider.Channel{ID: strconv.Itoa(live.ID), Name: live.Title}
		if logos := live.LogoImages["1x1"]; len(logos) > 0 {
			channel.Logo = "https:" + logos[0].URL // the API leaves the scheme out: //s.tvp.pl/…
		}
		channels = append(channels, channel)
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
				Start:       item.Since,
				Stop:        item.Till,
			})
		}
	}
	return programmes, nil
}

// Stream asks TVP for a fresh HLS URL of the channel. The URL carries a token
// that is only valid from the address that made this request.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	var playlist struct {
		Sources struct {
			HLS []struct{ Src string }
		}
		DRM map[string]json.RawMessage // license servers; present only on encrypted channels
	}
	path := "/" + url.PathEscape(channelID) + "/videos/playlist"
	if err := p.get(ctx, path, url.Values{"videoType": {"LIVE"}}, &playlist); err != nil {
		return provider.Source{}, fmt.Errorf("tvp: resolving stream: %w", err)
	}
	if len(playlist.DRM) > 0 {
		return provider.Source{}, errors.New("tvp: channel is DRM-protected")
	}
	if len(playlist.Sources.HLS) == 0 {
		return provider.Source{}, errors.New("tvp: channel has no HLS stream")
	}
	return provider.Source{URL: playlist.Sources.HLS[0].Src, Client: p.client}, nil
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
