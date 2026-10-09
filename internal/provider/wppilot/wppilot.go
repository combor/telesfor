// Package wppilot provides the live channels WP Pilot streams free to an
// account, such as Polsat, TV 4 and Telewizja WP. It uses the API behind
// pilot.wp.pl, as the site's own player does.
package wppilot

import (
	"bytes"
	"context"
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

	"github.com/combor/telesfor/internal/httpclient"
	"github.com/combor/telesfor/internal/provider"
)

const (
	siteURL = "https://pilot.wp.pl"

	// browser is who telesfor says it is. WP Pilot plays a free account's
	// channels in a browser only, and its stream servers answer a stream's
	// playlists only to who opened the stream. So every request names this
	// one: see agent.
	browser = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

	// beat is how often a stream's session is kept alive when WP does not say.
	beat = 20 * time.Second

	// grace is how long telling WP of a stream, or closing its session, may
	// take.
	grace = 10 * time.Second
)

// elsewhere are the channels that TVP's own tuner has: TVP 1, TVP 2, TVP
// Warszawa and TVP Gdańsk, by WP Pilot's numbers for them. The two services
// name them too differently to tell by name.
var elsewhere = []int{3, 5, 57, 187}

// Provider streams WP Pilot's free live channels.
type Provider struct {
	client *http.Client
	db     *bolt.DB // nil keeps the sign-in in memory only

	// The site, with its API under /api. Tests point it at a fake.
	site string

	poll     time.Duration // between two looks at a code the user has yet to enter
	codeLife time.Duration

	watching sync.WaitGroup // the sessions being kept: see keep

	mu       sync.Mutex
	account  *account         // nil when signed out
	expired  bool             // WP no longer accepts the account's session
	pending  *pending         // a code the user has yet to enter
	actions  int              // sign-ins and sign-outs so far: the last one stands
	problem  string           // what went wrong with the last sign-in, or stands in the way of playing
	viewings map[int]*viewing // the channels being watched, by WP's numbers
	closing  int              // sessions that nobody watches any more and WP has yet to be told to close
	closed   chan struct{}    // closed when there is none of them left; nil while there is none
	freed    int              // sessions closed so far
	changed  func()
}

// New returns a WP Pilot provider that keeps its sign-in in db.
//
// WP Pilot plays in Poland only. So when proxy is set, all traffic goes
// through it: the API calls made here and, later, the stream itself.
func New(proxy string, db *bolt.DB) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("wppilot: %w", err)
	}
	client.Transport = httpclient.Wrap(client.Transport, agent)
	p := &Provider{
		client:   client,
		db:       db,
		site:     siteURL,
		poll:     15 * time.Second, // as often as WP's own page for TV sets looks
		codeLife: 15 * time.Minute, // measured: WP does not say
	}
	if p.account, err = load(db); err != nil {
		return nil, err
	}
	return p, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "wppilot" }

// Channels lists the channels WP Pilot gave the account free when it was last
// asked, without those found since to be unplayable. There are none without
// an account.
//
// A channel's place is WP's number for it. So it keeps its number in Plex
// when others come and go, and when the account signs in anew. WP's numbers
// run to a few hundred, which the tuner's range has room for.
func (p *Provider) Channels(context.Context) ([]provider.Channel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account == nil {
		return nil, nil
	}
	var channels []provider.Channel
	for _, ch := range p.account.Channels {
		if !slices.Contains(p.account.Unplayable, ch.ID) {
			channels = append(channels, provider.Channel{ID: strconv.Itoa(ch.ID), Name: ch.Name, Logo: ch.Logo, Place: ch.ID})
		}
	}
	return channels, nil
}

// Stream returns the channel's HLS master playlist, with every quality in
// it, of a stream that WP has opened for the account.
//
// To WP a stream is a session of the account, of which it allows three at
// once and which it wants told, time and again, that somebody still watches.
// A channel has one session for all who watch it, opened for the first and
// closed after the last, whose staying is as long as their ctx: see viewing.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	p.mu.Lock()
	signedIn := p.account
	var ch channel
	if signedIn != nil {
		if i := slices.IndexFunc(signedIn.Channels, func(c channel) bool { return strconv.Itoa(c.ID) == channelID }); i >= 0 {
			ch = signedIn.Channels[i]
		}
	}
	switch {
	case signedIn == nil:
		p.mu.Unlock()
		return provider.Source{}, errors.New("wppilot: sign in to WP Pilot on telesfor's settings page")
	case ch.ID == 0:
		p.mu.Unlock()
		return provider.Source{}, fmt.Errorf("wppilot: no channel %q", channelID)
	}
	watched := p.viewings[ch.ID]
	// Not a session of an account that has signed out since.
	first := watched == nil || watched.account != signedIn
	if first {
		watched = &viewing{account: signedIn, opened: make(chan struct{}), over: make(chan struct{})}
		if p.viewings == nil {
			p.viewings = map[int]*viewing{}
		}
		p.viewings[ch.ID] = watched
	}
	watched.viewers++
	p.mu.Unlock()

	if first {
		// Not cut short by a viewer who leaves: a session opened at WP that
		// telesfor never heard of is one it cannot close, and others may be
		// waiting for this one.
		watched.session, watched.err = p.open(context.WithoutCancel(ctx), signedIn, ch)
		close(watched.opened)
		if watched.err == nil {
			p.watching.Go(func() { p.keep(ch, watched) })
		}
	} else {
		select {
		case <-watched.opened:
		case <-ctx.Done():
			p.leave(ch, watched)
			return provider.Source{}, fmt.Errorf("wppilot: %w", ctx.Err())
		}
	}
	if watched.err != nil {
		p.leave(ch, watched)
		return provider.Source{}, fmt.Errorf("wppilot: %w", watched.err)
	}
	context.AfterFunc(ctx, func() { p.leave(ch, watched) })
	return provider.Source{URL: watched.master, Client: p.client}, nil
}

// viewing is a channel that somebody watches, with the session that WP has
// open for it.
//
// The viewers of a channel share one session. WP stops every stream of a
// channel once the newest of the channel's sessions is closed, whoever
// watches the others: with a session each, a viewer who came later and left
// sooner would take the channel from the rest.
type viewing struct {
	account *account      // whose session it is
	opened  chan struct{} // closed once WP has answered:
	session               // with the session,
	err     error         // or why there is none
	viewers int           // guarded by the provider's mu
	over    chan struct{} // closed when the last viewer has left
}

// leave counts a viewer of a channel out.
func (p *Provider) leave(ch channel, watched *viewing) {
	p.mu.Lock()
	watched.viewers--
	last := watched.viewers == 0
	if last && p.viewings[ch.ID] == watched {
		delete(p.viewings, ch.ID)
	}
	if last && watched.err == nil { // with a session, which keep now closes
		if p.closing++; p.closing == 1 {
			p.closed = make(chan struct{})
		}
	}
	p.mu.Unlock()
	if last {
		close(watched.over)
	}
}

// vacated waits for the sessions that are being closed at WP, and tells
// whether any has been closed since freed stood at since.
func (p *Provider) vacated(since int) bool {
	p.mu.Lock()
	closed, freed := p.closed, p.freed
	p.mu.Unlock()
	if closed == nil {
		return freed != since
	}
	select {
	case <-closed:
	case <-time.After(2 * grace): // a sign of life on its way, then the closing
	}
	return true
}

// session is a stream that WP has opened for the account.
type session struct {
	token    string        // WP's name for it
	pulse    string        // where to tell WP that it is still watched
	every    time.Duration // and how often
	master   string        // its HLS master playlist; empty if it has none
	licensed bool          // WP names DRM licence servers for it
}

// open asks WP to open a stream of the channel for the account. A channel
// that does not open is told apart by what would help, and one that telesfor
// cannot play leaves the lineup.
func (p *Provider) open(ctx context.Context, signedIn *account, ch channel) (session, error) {
	var stream struct {
		Token     string
		Heartbeat struct {
			Interval float64
			URL      string
		}
		Channel struct {
			Streams []struct {
				Type string
				URL  []string
			}
			DRMs map[string]json.RawMessage `json:"drms"`
		} `json:"stream_channel"`
	}
	address := p.site + "/api/v3/channel/" + strconv.Itoa(ch.ID) + "?device_type=web"
	p.mu.Lock()
	freed := p.freed
	p.mu.Unlock()
	answer, err := p.call(ctx, signedIn, http.MethodGet, address, nil, &stream)
	// One of the streams that fill the account may be of a viewer who has
	// just left, as on a change of channel: once it is closed, there is room.
	if err == nil && answer.refused == "multiroom_limit_exceeded" && p.vacated(freed) {
		answer, err = p.call(ctx, signedIn, http.MethodGet, address, nil, &stream)
	}
	switch refused := answer.refused; {
	case err != nil:
		if stream.Token != "" { // opened all the same, by an answer that cannot be read
			p.shut(signedIn, ch, stream.Token)
		}
		return session{}, fmt.Errorf("resolving stream: %w", err)
	case refused == "not_authorized":
		return session{}, errExpired
	case refused == "rodo_agreements_required":
		p.settle(signedIn, true)
		return session{}, errors.New("the account has yet to accept WP Pilot's consents: see telesfor's settings page")
	case refused == "user_outside_eu" || refused == "user_not_verified_eu":
		return session{}, fmt.Errorf("%s is blocked outside Poland: set -wppilot-proxy to a proxy with a Polish exit, or try another exit", ch.Name)
	case refused == "user_channel_proxy_detected":
		return session{}, fmt.Errorf("%s is refused at this address, as WP Pilot does to VPNs: try another exit", ch.Name)
	case refused == "multiroom_limit_exceeded":
		return session{}, fmt.Errorf("%s cannot play: the account is at WP Pilot's limit of streams, with %s playing", ch.Name, strings.Join(answer.playing, ", "))
	case refused != "" || answer.status != http.StatusOK || stream.Token == "":
		return session{}, fmt.Errorf("%s is unavailable: WP Pilot answers %s", ch.Name, strings.TrimSpace(strconv.Itoa(answer.status)+" "+refused))
	}

	// WP counts the changes of channel an account without a package makes.
	if answer.left != nil {
		slog.Debug("wppilot: opened", "channel", ch.Name, "switches", *answer.left)
		if *answer.left == 0 {
			slog.Warn("wppilot: WP Pilot counts no change of channel left for the account")
		}
	}
	opened := session{
		token:    stream.Token,
		pulse:    stream.Heartbeat.URL,
		every:    seconds(stream.Heartbeat.Interval),
		licensed: len(stream.Channel.DRMs) > 0,
	}
	if opened.every <= 0 {
		opened.every = beat
	}
	for _, s := range stream.Channel.Streams {
		if strings.HasPrefix(s.Type, "hls") && len(s.URL) > 0 && opened.master == "" {
			opened.master = s.URL[0]
		}
	}

	unplayable := ""
	switch {
	case opened.master == "":
		unplayable = "has no HLS stream"
	case opened.licensed:
		// WP names licence servers for channels it streams in the clear as
		// well. The playlists tell.
		encrypted, err := p.encrypted(ctx, opened.master)
		if err != nil {
			p.shut(signedIn, ch, opened.token)
			return session{}, fmt.Errorf("%s is unavailable: reading its playlist: %w", ch.Name, err)
		}
		if encrypted {
			unplayable = "is DRM-protected"
		}
	}
	if unplayable != "" {
		p.shut(signedIn, ch, opened.token)
		p.drop(signedIn, ch)
		return session{}, fmt.Errorf("%s %s", ch.Name, unplayable)
	}
	p.settle(signedIn, false)
	return opened, nil
}

// encrypted tells whether a stream is DRM-protected, by its master playlist
// and the playlist of its best quality.
func (p *Provider) encrypted(ctx context.Context, master string) (bool, error) {
	fetch := func(address string) (string, *http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return "", nil, err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return "", nil, errors.Unwrap(err) // without the URL, which carries the stream's token
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", nil, errors.New(http.StatusText(resp.StatusCode))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return string(body), resp, err
	}
	playlists, resp, err := fetch(master)
	if err != nil {
		return false, err
	}
	if uri := provider.BestQuality(playlists); uri != "" {
		address, err := resp.Request.URL.Parse(uri)
		if err != nil {
			return false, err
		}
		quality, _, err := fetch(address.String())
		if err != nil {
			return false, err
		}
		playlists += quality
	}
	// AES-128 is no obstacle: its key is there for ffmpeg to fetch.
	return strings.Contains(playlists, "METHOD=SAMPLE-AES"), nil
}

// keep looks after a channel's session for as long as somebody watches the
// channel: it tells WP that the stream is still watched, as often as WP
// asked, and closes the session when the last viewer has left. A session that
// is not closed holds one of the account's three for minutes more.
func (p *Provider) keep(ch channel, watched *viewing) {
	warned := false
	for {
		select {
		case <-watched.over:
			p.shut(watched.account, ch, watched.token)
			p.mu.Lock()
			p.freed++
			if p.closing--; p.closing == 0 {
				close(p.closed)
				p.closed = nil
			}
			p.mu.Unlock()
			return
		case <-time.After(watched.every):
		}
		if watched.pulse == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		answer, err := p.call(ctx, watched.account, http.MethodPost, watched.pulse, struct{}{}, nil)
		cancel()
		// Once is enough: a session WP has dropped is refused every time.
		if refused := err != nil || answer.status != http.StatusNoContent && answer.status != http.StatusOK; refused && !warned {
			warned = true
			slog.Warn("wppilot: WP Pilot did not take a stream's sign of life", "channel", ch.Name,
				"answer", strings.TrimSpace(strconv.Itoa(answer.status)+" "+answer.refused), "err", errors.Unwrap(err))
		}
	}
}

// shut closes a stream's session at WP.
func (p *Provider) shut(signedIn *account, ch channel, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	answer, err := p.call(ctx, signedIn, http.MethodPost, p.site+"/api/v2/channels/close?device_type=web", map[string]string{"token": token}, nil)
	if err != nil || answer.status != http.StatusOK {
		slog.Warn("wppilot: a stream's session was not closed, and holds one of the account's for a few minutes", "channel", ch.Name,
			"answer", strings.TrimSpace(strconv.Itoa(answer.status)+" "+answer.refused), "err", err)
		return
	}
	slog.Debug("wppilot: closed", "channel", ch.Name)
}

// drop takes a channel that telesfor cannot play off the account's lineup,
// for as long as the account stays signed in.
func (p *Provider) drop(signedIn *account, ch channel) {
	p.mu.Lock()
	if p.account != signedIn || slices.Contains(signedIn.Unplayable, ch.ID) {
		p.mu.Unlock()
		return
	}
	signedIn.Unplayable = append(signedIn.Unplayable, ch.ID)
	err := save(p.db, signedIn)
	changed := p.changed
	p.mu.Unlock()

	if err != nil {
		slog.Error("wppilot: saving the channels", "err", err)
	}
	if changed != nil {
		changed()
	}
}

// answer is what WP's API says beside the data it was asked for.
type answer struct {
	status  int
	refused string   // WP's name for why not, of a refusal
	playing []string // the channels that an account at its limit of streams plays
	left    *int     // the changes of channel WP counts as left, if it says
	id, val string   // the session's cookies, if the answer sets them
}

// call sends a request to WP's API, as the account if there is one, with body
// as JSON if there is one, and decodes the data of a JSON answer into v. The
// refusals are JSON as well: see answer.
//
// An account's session is two cookies, which WP's servers reissue as they
// see fit. So call also takes up the ones an answer sets, and notes when WP
// no longer knows the session.
func (p *Provider) call(ctx context.Context, signedIn *account, method, address string, body, v any) (answer, error) {
	got, sent, err := p.send(ctx, signedIn, method, address, body, v)
	if signedIn == nil {
		return got, err
	}
	// Refused with a session that the answer to another call has reissued
	// meanwhile: that says nothing of the session as it is now.
	if err == nil && got.refused == "not_authorized" && p.cookies(signedIn) != sent {
		got, _, err = p.send(ctx, signedIn, method, address, body, v)
	}
	if err == nil && got.refused == "not_authorized" {
		p.expire(signedIn)
	}
	return got, err
}

// cookies returns an account's session as a request carries it.
func (p *Provider) cookies(signedIn *account) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return sessionID + "=" + signedIn.ID + "; " + sessionVal + "=" + signedIn.Val
}

// send is one request of call. It also returns the session it was sent with.
func (p *Provider) send(ctx context.Context, signedIn *account, method, address string, body, v any) (got answer, sent string, err error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return answer{}, "", err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, payload)
	if err != nil {
		return answer{}, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// As the site's own player asks. WP's API refuses a request from nowhere.
	req.Header.Set("Origin", p.site)
	req.Header.Set("Referer", p.site+"/tv/")
	// The session is between telesfor and the API alone, wherever WP says a
	// stream's sign of life is to be sent.
	api := strings.HasPrefix(address, p.site+"/api/")
	if signedIn != nil && api {
		sent = p.cookies(signedIn)
		req.Header.Set("Cookie", sent)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return answer{}, sent, err
	}
	defer resp.Body.Close()

	got = answer{status: resp.StatusCode}
	for _, cookie := range resp.Cookies() {
		// Not one that takes the session away, nor a guest's, which WP hands
		// to a caller it does not know.
		if !api || cookie.Value == "" || cookie.MaxAge < 0 || strings.HasPrefix(cookie.Value, "g:") {
			continue
		}
		switch cookie.Name {
		case sessionID:
			got.id = cookie.Value
		case sessionVal:
			got.val = cookie.Value
		}
	}
	if signedIn != nil {
		p.renew(signedIn, got.id, got.val)
	}
	if resp.StatusCode == http.StatusNoContent {
		return got, sent, nil
	}

	var envelope struct {
		Data json.RawMessage
		Meta struct {
			Error *struct {
				Name string
				Info json.RawMessage // of a limit of streams: the streams that play
			}
			Switches *struct{ Left int }
		} `json:"_meta"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		if resp.StatusCode == http.StatusOK {
			return got, sent, fmt.Errorf("reading the answer: %w", err)
		}
		return got, sent, nil
	}
	if refusal := envelope.Meta.Error; refusal != nil {
		got.refused = refusal.Name
		var limit struct {
			Streams []struct {
				Channel string `json:"channel_name"`
			}
		}
		json.Unmarshal(refusal.Info, &limit) // of another refusal, the info is none, or something else
		for _, stream := range limit.Streams {
			got.playing = append(got.playing, stream.Channel)
		}
	}
	if envelope.Meta.Switches != nil {
		got.left = &envelope.Meta.Switches.Left
	}
	if v != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, v); err != nil {
			return got, sent, fmt.Errorf("reading the answer: %w", err)
		}
	}
	return got, sent, nil
}

// agent sends the requests of the provider's HTTP client. It names the
// browser in every one, to WP's API and to its stream servers alike.
func agent(req *http.Request, next http.RoundTripper) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("User-Agent", browser)
	return next.RoundTrip(out)
}

func seconds(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }
