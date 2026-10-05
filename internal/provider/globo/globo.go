// Package globo provides the live channels Globoplay streams free to a Globo
// account: TV Globo, Futura and ge tv. It uses the APIs behind
// globoplay.globo.com.
package globo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
)

const (
	loginURL     = "https://login.globo.com"
	graphqlURL   = "https://multicloud-jarvis.globo.com/graphql"
	playbackURL  = "https://playback.video.globo.com/v5/video-session"
	affiliateURL = "https://affiliates.video.globo.com/affiliates/info"

	// service is Globoplay among the services of a Globo account.
	service = "4654"
)

// slugs name the channels to offer. Globoplay lists more, most of which need a
// subscription. A channel's place here is its place in the lineup for good:
// add new ones at the end.
var slugs = []string{"tv-globo", "futura", "ge-tv"}

// brt is the time of Brasília, which Globo's guide is laid out in. Brazil has
// no summer time.
var brt = time.FixedZone("BRT", -3*60*60)

// Provider streams Globoplay's free live channels.
type Provider struct {
	client *http.Client
	db     *bolt.DB // nil keeps the sign-in in memory only

	// The APIs. Tests point them at a fake.
	login, graphql, playback, affiliate string

	poll     time.Duration // between two looks at a code the user has yet to enter
	codeLife time.Duration

	mu      sync.Mutex
	account *account // nil when signed out
	expired bool     // Globo no longer accepts the account's session
	pending *pending // a code the user has yet to enter
	actions int      // sign-ins and sign-outs so far: the last one stands
	problem string   // what went wrong with the last sign-in
	changed func()
}

// channel is a channel as Globoplay lists it.
type channel struct {
	ID    string `json:"id"` // the slug: Globo's name for it in URLs
	Name  string `json:"name"`
	Logo  string `json:"logo,omitempty"`
	Media string `json:"media"` // id of its live video; the same for every region
}

// New returns a Globoplay provider that keeps its sign-in in db.
//
// Globo blocks its channels outside Brazil, and picks the regional feed of TV
// Globo by the address that asks. So when proxy is set, all traffic goes
// through it: the API calls made here and, later, the stream itself.
func New(proxy string, db *bolt.DB) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("globo: %w", err)
	}
	p := &Provider{
		client:    client,
		db:        db,
		login:     loginURL,
		graphql:   graphqlURL,
		playback:  playbackURL,
		affiliate: affiliateURL,
		poll:      5 * time.Second, // as often as Globo's own page looks
		codeLife:  5 * time.Minute, // measured: Globo does not say
	}
	if p.account, err = load(db); err != nil {
		return nil, err
	}
	return p, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "globo" }

// Channels lists the channels as they were when the account signed in. There
// are none without an account.
func (p *Provider) Channels(context.Context) ([]provider.Channel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account == nil {
		return nil, nil
	}
	var channels []provider.Channel
	for _, ch := range p.account.Channels {
		// Not a channel that telesfor has stopped offering since the sign-in.
		if place := slices.Index(slugs, ch.ID) + 1; place > 0 {
			channels = append(channels, provider.Channel{ID: ch.ID, Name: ch.Name, Logo: ch.Logo, Place: place})
		}
	}
	return channels, nil
}

// Programmes fetches the guide, which Globo lays out in days, in one request.
//
// It is also when telesfor looks after the account: playing a free channel
// does not tell whether Globo still accepts the sign-in, and Plex asks for the
// guide every day.
func (p *Provider) Programmes(ctx context.Context, channels []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	p.verify(ctx)
	media := map[string]string{}
	p.mu.Lock()
	if p.account != nil {
		for _, ch := range p.account.Channels {
			media[ch.ID] = ch.Media
		}
	}
	p.mu.Unlock()

	// Aliases tell the answers apart: c0 is the first channel, d0 its first day.
	var query strings.Builder
	query.WriteString("query($region: BroadcastInput) { all: broadcasts { slug name mediaId logo }")
	year, month, day := from.In(brt).Date()
	first, last := time.Date(year, month, day, 0, 0, 0, 0, brt), to.In(brt)
	for i, ch := range channels {
		fmt.Fprintf(&query, " c%d: broadcast(mediaId: %q, filtersInput: $region) {", i, media[ch.ID])
		for d, day := 0, first; !day.After(last); d, day = d+1, day.AddDate(0, 0, 1) {
			fmt.Fprintf(&query, " d%d: epgByDate(date: %q) { entries { name description startTime endTime title { poster { web } } } }",
				d, day.Format(time.DateOnly))
		}
		query.WriteString(" }")
	}
	query.WriteString(" }")

	var guide map[string]json.RawMessage
	err := p.query(ctx, query.String(), map[string]any{"region": p.region(ctx)}, &guide)
	// Even from an answer that is refused: asked about a video it no longer
	// has, Globo sends an error beside the list that names the new one.
	p.relist(guide["all"])
	if err != nil {
		return nil, fmt.Errorf("globo: fetching guide: %w", err)
	}

	var programmes []provider.Programme
	for i, ch := range channels {
		var days map[string]struct {
			Entries []struct {
				Name, Description  string
				StartTime, EndTime int64
				Title              struct{ Poster struct{ Web string } }
			}
		}
		if err := json.Unmarshal(guide["c"+strconv.Itoa(i)], &days); err != nil {
			return nil, fmt.Errorf("globo: reading the guide of %s: %w", ch.Name, err)
		}
		listed := len(programmes)
		seen := map[int64]bool{} // a programme on air at midnight is in both days
		for d := range len(days) {
			for _, entry := range days["d"+strconv.Itoa(d)].Entries {
				start, stop := time.Unix(entry.StartTime, 0).In(brt), time.Unix(entry.EndTime, 0).In(brt)
				if stop.Second() == 59 { // Globo ends a programme a second before the next
					stop = stop.Add(time.Second)
				}
				if seen[entry.StartTime] || !stop.After(from) || !start.Before(to) {
					continue
				}
				seen[entry.StartTime] = true
				programmes = append(programmes, provider.Programme{
					ChannelID:   ch.ID,
					Title:       entry.Name,
					Description: entry.Description,
					Image:       entry.Title.Poster.Web,
					Start:       start,
					Stop:        stop,
				})
			}
		}
		// Globo's answer can lack a channel without saying why. A guide with a
		// channel missing would wipe what Plex has of it.
		if len(programmes) == listed {
			return nil, fmt.Errorf("globo: fetching guide: nothing on %s", ch.Name)
		}
	}
	return programmes, nil
}

// region is the filter that gets TV Globo's guide for the regional station
// Globo serves to this address. Without one, the guide is the network's.
func (p *Provider) region(ctx context.Context) map[string]string {
	var station struct{ Code string }
	if status, err := p.call(ctx, http.MethodGet, p.affiliate, nil, nil, &station); err != nil || status != http.StatusOK || station.Code == "" {
		return nil
	}
	return map[string]string{"affiliateCode": station.Code}
}

// list picks the channels to offer from Globoplay's, in the order of slugs.
func list(broadcasts json.RawMessage) []channel {
	var all []struct{ Slug, Name, MediaID, Logo string }
	json.Unmarshal(broadcasts, &all) // none is the answer to a list that cannot be read
	var channels []channel
	for _, slug := range slugs {
		for _, b := range all {
			if b.Slug == slug && b.MediaID != "" {
				channels = append(channels, channel{ID: b.Slug, Name: b.Name, Logo: b.Logo, Media: b.MediaID})
			}
		}
	}
	return channels
}

// merge brings known channels up to date with a list of Globoplay's. A
// channel the list lacks stays: Globo's lists come up short at times.
func merge(known, listed []channel) []channel {
	channels := slices.Clone(listed)
	for _, ch := range known {
		if !slices.ContainsFunc(listed, func(l channel) bool { return l.ID == ch.ID }) {
			channels = append(channels, ch)
		}
	}
	slices.SortFunc(channels, func(a, b channel) int { return slices.Index(slugs, a.ID) - slices.Index(slugs, b.ID) })
	return channels
}

// relist brings the account's channels up to date with Globoplay's list.
func (p *Provider) relist(broadcasts json.RawMessage) {
	listed := list(broadcasts)
	p.mu.Lock()
	if p.account == nil {
		p.mu.Unlock()
		return
	}
	channels := merge(p.account.Channels, listed)
	if slices.Equal(channels, p.account.Channels) {
		p.mu.Unlock()
		return
	}
	p.account.Channels = channels
	err := save(p.db, p.account)
	changed := p.changed
	p.mu.Unlock()

	if err != nil {
		slog.Error("globo: saving the channels", "err", err)
	}
	if changed != nil {
		changed()
	}
}

// Stream asks Globo for a fresh HLS URL of the channel, and returns its best
// quality. The URL carries a token of its own, so fetching the stream takes no
// sign-in.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	var media string
	p.mu.Lock()
	signedIn := p.account
	if signedIn != nil {
		for _, ch := range signedIn.Channels {
			if ch.ID == channelID {
				media = ch.Media
			}
		}
	}
	p.mu.Unlock()
	if media == "" {
		return provider.Source{}, errors.New("globo: sign in to Globoplay on telesfor's settings page")
	}

	// Without the dvr capability, the playlist holds two minutes rather than
	// the last hour and a half, and takes that much less to reload.
	request := map[string]any{
		"player_type":        "desktop",
		"video_id":           media,
		"quality":            "max",
		"content_protection": "widevine", // required; clear streams come back all the same
		"vsid":               uuid(),
		"consumption":        "streaming",
		"tz":                 "-03:00",
		"version":            2,
	}
	var answer struct {
		Code     string // of a refusal
		Sources  []struct{ URL string }
		Resource struct {
			DRM bool `json:"drm_protection_enabled"`
		}
	}
	status, err := p.call(ctx, http.MethodPost, p.playback, map[string]string{"Authorization": "Bearer " + signedIn.GLBID}, request, &answer)
	switch {
	case err != nil:
		return provider.Source{}, fmt.Errorf("globo: resolving stream: %w", err)
	case answer.Code == "geo-block" || answer.Code == "geo-fencing":
		return provider.Source{}, errors.New("globo: blocked outside Brazil: set -globo-proxy to a proxy with a Brazilian exit, or try another exit")
	case answer.Code == "login-required":
		p.expire(signedIn)
		return provider.Source{}, errors.New("globo: the sign-in has expired: sign in again on telesfor's settings page")
	case answer.Code == "user-not-authorized":
		return provider.Source{}, errors.New("globo: the account has no access to this channel")
	case status != http.StatusOK:
		return provider.Source{}, fmt.Errorf("globo: resolving stream: %s", strings.TrimSpace(http.StatusText(status)+" "+answer.Code))
	case answer.Resource.DRM:
		return provider.Source{}, errors.New("globo: channel is DRM-protected")
	case len(answer.Sources) == 0 || answer.Sources[0].URL == "":
		return provider.Source{}, errors.New("globo: channel has no stream")
	}
	best, err := p.best(ctx, answer.Sources[0].URL)
	if err != nil {
		return provider.Source{}, fmt.Errorf("globo: reading the stream's qualities: %w", err)
	}
	return provider.Source{URL: best, Client: p.client}, nil
}

// best returns the playlist of the highest quality in a master playlist.
//
// Every quality of Globo's has the sound in it. Given them all, ffmpeg takes
// the picture from the best and the sound from the first, and so downloads
// two.
func (p *Provider) best(ctx context.Context, master string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, master, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", errors.Unwrap(err) // without the URL, which carries the stream's token
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	best, most, next := "", -1, -1 // next is the bandwidth of the quality whose URI comes next
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if attributes, ok := strings.CutPrefix(line, "#EXT-X-STREAM-INF:"); ok {
			next = 0
			for attribute := range strings.SplitSeq(attributes, ",") {
				if bandwidth, ok := strings.CutPrefix(attribute, "BANDWIDTH="); ok {
					next, _ = strconv.Atoi(bandwidth)
				}
			}
		} else if line != "" && !strings.HasPrefix(line, "#") && next >= 0 {
			if next > most {
				best, most = line, next
			}
			next = -1
		}
	}
	if err := lines.Err(); err != nil {
		return "", err
	}
	if best == "" {
		return master, nil // not a master playlist: the stream has one quality
	}
	playlist, err := resp.Request.URL.Parse(best) // after redirects
	if err != nil {
		return "", err
	}
	return playlist.String(), nil
}

// query asks Globoplay's GraphQL API and decodes the data of its answer into
// v. An answer with errors is refused, with the data it has decoded all the
// same.
func (p *Provider) query(ctx context.Context, query string, variables map[string]any, v any) error {
	// The API refuses requests that do not name all four.
	header := map[string]string{
		"x-tenant-id":      "globo-play",
		"x-platform-id":    "web",
		"x-device-id":      "desktop",
		"x-client-version": "telesfor",
	}
	var answer struct {
		Data   json.RawMessage
		Errors []struct{ Message string }
	}
	status, err := p.call(ctx, http.MethodPost, p.graphql, header, map[string]any{"query": query, "variables": variables}, &answer)
	if err != nil {
		return err
	}
	if len(answer.Data) > 0 {
		if err := json.Unmarshal(answer.Data, v); err != nil {
			return err
		}
	}
	switch {
	case len(answer.Errors) > 0: // even beside data, which is then partial
		return errors.New(answer.Errors[0].Message)
	case status != http.StatusOK || len(answer.Data) == 0:
		return errors.New(http.StatusText(status))
	}
	return nil
}

// call sends a request, with body as JSON if there is one, and decodes a JSON
// answer into v. It returns the answer's status: Globo's refusals are JSON as
// well, or empty.
func (p *Provider) call(ctx context.Context, method, url string, header map[string]string, body, v any) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
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

	if v == nil {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil && resp.StatusCode == http.StatusOK {
		return resp.StatusCode, fmt.Errorf("reading the answer: %w", err)
	}
	return resp.StatusCode, nil
}

// uuid returns a random UUID, which is how a player names its session.
func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80 // version 4
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
