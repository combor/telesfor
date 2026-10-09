// Package tf1 provides the live channels that TF1+ streams unencrypted: TF1,
// TFX and TF1 Séries Films to a TF1+ account, which is free, and LCI to
// anyone. It uses the APIs behind tf1.fr, and the guide TF1 publishes for the
// press.
package tf1

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/httpclient"
	"github.com/combor/telesfor/internal/provider"
)

const (
	// siteURL is tf1.fr, where an account signs in: see login.go.
	siteURL = "https://www.tf1.fr"

	// playerURL is where TF1's player asks for a video.
	playerURL = "https://mediainfo.tf1.fr/mediainfocombo"

	// guideURL is the guide TF1 publishes for the press: see guide.go.
	guideURL = "https://tf1pro.com/grilles-tv"

	// phone is the browser telesfor says it is to the player's API. An
	// iPhone's is told where the HLS stream is, any other where the DASH one
	// is.
	phone = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1"
)

// channel is a channel as TF1+ streams it.
type channel struct {
	id    string // its name in telesfor's addresses
	live  string // the id of its live video
	name  string
	logo  string
	guide string // its name in the addresses of TF1's guide; empty where the guide has nothing of it
	free  bool   // it plays without an account
}

// channels are the channels to offer. A channel's place here is its place in
// the lineup for good: add new ones at the end.
//
// TF1+ streams more than these: TMC, which is DRM-protected, and channels it
// makes for the web.
var channels = []channel{
	{"tf1", "L_TF1", "TF1", "https://photos.tf1.fr/450/0/logo-tf1-2020-min-1c7c27-26ba3a-0@1x.jpg", "TF1", false},
	{"tfx", "L_TFX", "TFX", "https://photos.tf1.fr/450/0/logo-tfx-2020-min-2ac5f0-c28f82-0@1x.jpg", "TFX", false},
	{"tf1-series-films", "L_TF1-SERIES-FILMS", "TF1 Séries Films", "https://photos.tf1.fr/450/0/logo-tf1sf-2020-min-4957cb-17d714-0@1x.jpg", "TF1-SERIES-FILMS", false},
	{"lci", "L_LCI", "LCI", "https://photos.tf1.fr/450/0/logo-lci-2020-min-a0978b-4a05fe-0@1x.jpg", "", true},
}

// Provider streams TF1+'s live channels.
type Provider struct {
	client *http.Client
	db     *bolt.DB // nil keeps the sign-in in memory only

	// The APIs. Tests point them at a fake.
	site, player, guide string

	slower time.Duration // what TF1 asking for patience adds to the time between two looks at a code
	rest   time.Duration // see provider.Rest; tests are in more of a hurry

	renewing sync.Mutex // held while the account's tokens are renewed: see token

	mu      sync.Mutex
	account *account // nil when signed out
	expired bool     // TF1 no longer accepts the account's tokens
	pending *pending // a code the user has yet to enter
	actions int      // sign-ins and sign-outs so far: the last one stands
	problem string   // what went wrong with the last sign-in
	changed func()
}

// New returns a TF1+ provider that keeps its sign-in in db.
//
// TF1 keeps its channels to France, all but LCI, and tells by the address
// that asks for the stream and by the one that fetches it. So when proxy is
// set, all traffic goes through it: the API calls made here and, later, the
// stream itself.
func New(proxy string, db *bolt.DB) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("tf1: %w", err)
	}
	p := &Provider{
		client: client,
		db:     db,
		site:   siteURL,
		player: playerURL,
		guide:  guideURL,
		slower: 5 * time.Second, // as RFC 8628 has it
		rest:   provider.Rest,
	}
	if p.account, err = load(db); err != nil {
		return nil, fmt.Errorf("tf1: %w", err)
	}
	return p, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "tf1" }

// Channels lists the channels that play: all of them with an account, and
// without one those that need none.
func (p *Provider) Channels(context.Context) ([]provider.Channel, error) {
	p.mu.Lock()
	signedIn := p.account != nil
	p.mu.Unlock()
	var listed []provider.Channel
	for i, ch := range channels {
		if ch.free || signedIn {
			listed = append(listed, provider.Channel{ID: ch.id, Name: ch.name, Logo: ch.logo, Place: i + 1})
		}
	}
	return listed, nil
}

// Stream asks TF1 where the channel's stream is and returns its master
// playlist, with every quality in it, and a client that keeps the stream's
// pass good: see session.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	ch, ok := find(channelID)
	if !ok {
		return provider.Source{}, fmt.Errorf("tf1: no channel %q", channelID)
	}
	master, err := p.feed(ctx, ch)
	if err != nil {
		return provider.Source{}, fmt.Errorf("tf1: %w", err)
	}
	pass, stream := passOf(master)
	fresh := func(ctx context.Context) string {
		again, err := p.feed(ctx, ch)
		if err != nil {
			return ""
		}
		// A pass to another stream is none to this one.
		if pass, same := passOf(again); same == stream {
			return pass
		}
		return ""
	}
	s := session{provider.NewPass(pass, p.rest, fresh)}
	client := *p.client
	client.Transport = httpclient.Wrap(p.client.Transport, s.roundTrip)
	return provider.Source{URL: master, Client: &client}, nil
}

// video is what the player's API says of a channel's live video.
type video struct {
	Media struct {
		Error string `json:"error_code"` // of a refusal, which comes with a status of 200
	}
	Delivery struct {
		Code     int
		URL      string
		Format   string
		DRMs     []json.RawMessage
		Fallback struct{ URL string }
	}
}

// feed asks the player's API for the channel's stream, as the account if the
// channel takes one, and returns the address of its master playlist. A
// channel that does not play is told apart by what would help.
func (p *Provider) feed(ctx context.Context, ch channel) (string, error) {
	var token string
	if !ch.free {
		var err error
		if token, err = p.token(ctx, ""); err != nil {
			return "", err
		}
		if token == "" {
			return "", errors.New("sign in to TF1+ on telesfor's settings page")
		}
	}
	answer, err := p.ask(ctx, ch, token)
	// A token that TF1 will not take may still be renewed.
	if err == nil && answer.Media.Error == "AUTH_ERROR" && token != "" {
		if token, err = p.token(ctx, token); err != nil {
			return "", err
		}
		answer, err = p.ask(ctx, ch, token)
	}
	switch refused := answer.Media.Error; {
	case err != nil:
		return "", fmt.Errorf("resolving stream: %w", err)
	case refused == "GEOBLOCKED":
		return "", fmt.Errorf("%s is blocked outside France: set -tf1-proxy to a proxy with a French exit, or try another exit", ch.name)
	case refused == "AUTH_ERROR":
		p.expire(token)
		return "", errExpired
	case refused == "PERMISSION_DENIED" && token != "":
		return "", fmt.Errorf("the account has no access to %s", ch.name)
	case refused != "" || answer.Delivery.Code != http.StatusOK:
		return "", fmt.Errorf("%s is unavailable: TF1 answers %s", ch.name, strings.TrimSpace(strconv.Itoa(answer.Delivery.Code)+" "+refused))
	case len(answer.Delivery.DRMs) > 0:
		return "", fmt.Errorf("%s is DRM-protected", ch.name)
	case answer.Delivery.Format != "hls" || answer.Delivery.URL == "":
		return "", fmt.Errorf("%s has no HLS stream", ch.name)
	}
	// TF1 itself comes in two ways: with adverts put in for each viewer, in
	// playlists made for that viewer, and as it is broadcast, which TF1's
	// player falls back on. The other channels come as they are broadcast.
	// Only a stream of that kind goes on under another pass: see session.
	return cmp.Or(answer.Delivery.Fallback.URL, answer.Delivery.URL), nil
}

// ask asks the player's API for a channel's live video, with the account's
// token if there is one.
func (p *Provider) ask(ctx context.Context, ch channel, token string) (video, error) {
	header := map[string]string{"User-Agent": phone}
	if token != "" {
		header["Authorization"] = "Bearer " + token
	}
	// The API refuses requests that do not say which player asks. The
	// version goes with it, as the player sends it.
	query := url.Values{"context": {"MYTF1"}, "pver": {"5015000"}}
	var answer video
	status, err := p.call(ctx, http.MethodGet, p.player+"/"+url.PathEscape(ch.live)+"?"+query.Encode(), header, nil, &answer)
	if err == nil && status != http.StatusOK {
		err = errors.New(http.StatusText(status))
	}
	return answer, err
}

// passOf returns the pass in the address of a stream, which is the first step
// of its path, and what the address is without it: /pass and
// host/live/index.m3u8 of https://host/pass/live/index.m3u8. Both are empty
// for an address without one.
func passOf(address string) (pass, stream string) {
	at, err := url.Parse(address)
	if err != nil {
		return "", ""
	}
	first, file, ok := strings.Cut(strings.TrimPrefix(at.Path, "/"), "/")
	if !ok || first == "" {
		return "", ""
	}
	return "/" + first, at.Host + "/" + file
}

// session keeps a stream's pass good. TF1's lasts four hours.
type session struct{ *provider.Pass }

func (s session) roundTrip(req *http.Request, next http.RoundTripper) (*http.Response, error) {
	pass := s.Current()
	resp, err := s.Send(next, req, pass)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		return resp, err
	}

	// TF1's servers give their reason for a refusal in its body:
	// edge-vhost/invalid-token for a pass, edge-vhost/geoip-restriction for
	// an address outside France, which no pass makes up for.
	reason, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if !bytes.Contains(reason, []byte("geoip")) {
		if pass = s.Renew(req.Context(), pass); pass != "" {
			return s.Send(next, req, pass)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(reason))
	return resp, nil
}

// find looks a channel up by its id.
func find(id string) (channel, bool) {
	i := slices.IndexFunc(channels, func(ch channel) bool { return ch.id == id })
	if i < 0 {
		return channel{}, false
	}
	return channels[i], true
}

// call sends a request to one of the APIs, with body as JSON if there is
// one, and decodes a JSON answer into v. It returns the answer's status: the
// refusals are JSON as well.
func (p *Provider) call(ctx context.Context, method, address string, header map[string]string, body, v any) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, payload)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil && resp.StatusCode == http.StatusOK {
		return resp.StatusCode, fmt.Errorf("reading the answer: %w", err)
	}
	return resp.StatusCode, nil
}
