// Package francetv provides the live channels of France Télévisions, the
// French public broadcaster: France 2, France 3, France 4, France 5 and
// franceinfo. It uses the APIs behind france.tv and behind France
// Télévisions' apps, none of which asks for an account.
package francetv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/combor/telesfor/internal/httpclient"
	"github.com/combor/telesfor/internal/provider"
)

const (
	// guideURL is the guide that france.tv's own is made of, a day of a
	// channel at a time: ?date=2026-10-07&channel=france-2. It has what a
	// channel shows and when, but calls a programme by its episode alone,
	// such as "Émission du mercredi 7 octobre 2026".
	guideURL = "https://www.france.tv/api/epg/videos/"

	// appsURL is the API of France Télévisions' apps. It has the live video
	// of every channel, and knows a programme by its name: see learn.
	appsURL = "https://api-mobile.yatta.francetv.fr/generic"

	// playerURL is where france.tv's player asks for a video.
	playerURL = "https://k7.ftven.fr/videos"

	// lull is the longest time between two programmes that still goes to the
	// first: the guide leaves out trailers, adverts and the like.
	lull = 20 * time.Minute

	// lookups is how many programmes are looked up at once.
	lookups = 6

	// rest is how long a stream's pass is left alone once it is handed out:
	// see session.
	rest = time.Minute

	// unlisted is what a placeholder says of itself.
	unlisted = "France Télévisions has published no listings for this time."
)

// channel is a channel as France Télévisions streams it.
type channel struct {
	id   string // its name in France Télévisions' addresses
	name string
	logo string
}

// channels are the channels to offer. A channel's place here is its place in
// the lineup for good: add new ones at the end.
//
// france.tv streams more than these: channels it makes for the web, and those
// of other broadcasters, such as Arte. And France 3 is the national one,
// without the regional programmes.
var channels = []channel{
	{"france-2", "France 2", "https://medias.france.tv/v25lgE50IPcm5SkmaN7WLDiOuEo/400x400/filters:quality(85)/c/7/t/php6kjt7c.png"},
	{"france-3", "France 3", "https://medias.france.tv/2cSonvrdpEyEdrJ-mpSlq6VRyQo/400x400/filters:quality(85)/i/v/v/phpjxpvvi.png"},
	{"france-4", "France 4", "https://medias.france.tv/gPIpmplXDRY_1BHEJOKsrR7ZpPU/400x400/filters:quality(85)/g/7/f/php2isf7g.png"},
	{"france-5", "France 5", "https://medias.france.tv/hztfOht-0xZ-zZvXJoQtjd9s1IM/400x400/filters:quality(85)/3/v/x/phptcexv3.png"},
	{"franceinfo", "franceinfo", "https://medias.france.tv/E4mTVkgkoJVsZkf9Tj9Aw-Q8uNQ/400x400/filters:quality(85)/5/u/b/phpp1cbu5.jpg"},
}

// Provider streams France Télévisions' live channels.
type Provider struct {
	client *http.Client

	// The APIs. Tests point them at a fake.
	guide, apps, player string

	rest time.Duration // see the constant; tests are in more of a hurry

	learning sync.Mutex // held while programmes are looked up: see learn

	mu    sync.Mutex
	live  map[string]string // the id of each channel's live video: see video
	shows map[number]show   // the programmes looked up so far, by id: see learn
	asked map[string]bool   // where the apps' API has been asked for one
}

// New returns a France Télévisions provider.
//
// France Télévisions keeps its channels to France, all but franceinfo, and
// tells by the address that fetches the stream. So when proxy is set, all
// traffic goes through it: the API calls made here and, later, the stream
// itself.
func New(proxy string) (*Provider, error) {
	client, err := provider.Client(proxy)
	if err != nil {
		return nil, fmt.Errorf("francetv: %w", err)
	}
	return &Provider{
		client: client,
		guide:  guideURL,
		apps:   appsURL,
		player: playerURL,
		rest:   rest,
		live:   map[string]string{},
		shows:  map[number]show{},
		asked:  map[string]bool{},
	}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "francetv" }

// Channels lists the channels, which are always the same.
func (p *Provider) Channels(context.Context) ([]provider.Channel, error) {
	listed := make([]provider.Channel, len(channels))
	for i, ch := range channels {
		listed[i] = provider.Channel{ID: ch.id, Name: ch.name, Logo: ch.logo, Place: i + 1}
	}
	return listed, nil
}

// number is an id in France Télévisions' answers, which write the same one as
// a number here and as a string there.
type number string

func (n *number) UnmarshalJSON(b []byte) error {
	if *n = number(strings.Trim(string(b), `"`)); *n == "null" {
		*n = ""
	}
	return nil
}

// entry is a programme as france.tv's guide lists it.
type entry struct {
	Content struct {
		ID          number
		Title       string    // of the episode, mostly: "S1 E3 - L'autre vie d'Elodie"
		Description string    // of the episode as well
		URL         string    // its page, if it has one: /france-2/telematin/8866926-emission-du-….html
		Start       time.Time `json:"isoDate"`
		Length      string    `json:"isoDuration"` // as in PT1H25M0S
		Images      struct {
			Base     struct{ X1, X2 string }
			Expanded struct{ X1 string }
		}
	}
	// What the page reports to France Télévisions' statistics, the one place
	// where the guide tells a programme from its episodes.
	Tracking struct {
		Program   string // "telematin", or "le_fil_info|le_fil_info_matin"
		ProgramID number `json:"program_id"`
	}
}

// length is how long the entry lasts, or zero if the guide does not say.
func (e entry) length() time.Duration {
	length, _ := time.ParseDuration(strings.ToLower(strings.TrimPrefix(e.Content.Length, "PT")))
	return length
}

// paths are where the apps' API may have the entry's programme. It keeps one
// under the start of the address of its episodes' pages, as france-2_telematin
// for /france-2/telematin/8866926-….html, at a depth that varies. An entry
// without a page leaves the channel's name and the programme's to try.
func (e entry) paths(channelID string) []string {
	var paths []string
	if parts := strings.Split(strings.Trim(e.Content.URL, "/"), "/"); strings.HasPrefix(e.Content.URL, "/") {
		// Without the page itself. At a depth of one is a channel, or a kind
		// of programme.
		for depth := 2; depth < len(parts); depth++ {
			paths = append(paths, strings.Join(parts[:depth], "_"))
		}
	}
	if slug, _, _ := strings.Cut(e.Tracking.Program, "|"); slug != "" {
		paths = append(paths, channelID+"_"+strings.ReplaceAll(slug, "_", "-"))
	}
	return paths
}

// show is a programme as the apps' API knows it.
type show struct {
	name   string
	poster string // an upright picture of it; optional
}

// Programmes fetches the guide of the channels, and the names of the
// programmes in it that are not known yet.
func (p *Provider) Programmes(ctx context.Context, channels []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	listed := make([][]entry, len(channels))
	failed := make([]error, len(channels))
	var fetching sync.WaitGroup
	for i, ch := range channels {
		fetching.Go(func() { listed[i], failed[i] = p.listings(ctx, ch, from, to) })
	}
	fetching.Wait()
	for _, err := range failed {
		if err != nil {
			return nil, fmt.Errorf("francetv: fetching guide: %w", err)
		}
	}
	if err := p.learn(ctx, channels, listed); err != nil {
		return nil, fmt.Errorf("francetv: fetching guide: %w", err)
	}

	p.mu.Lock()
	shows := maps.Clone(p.shows)
	p.mu.Unlock()
	var programmes []provider.Programme
	for i, ch := range channels {
		known := schedule(ch, listed[i], shows, from, to)
		// As when france.tv lays its guide out anew. Placeholders alone would
		// wipe what Plex has of the channel.
		if len(known) == 0 {
			return nil, fmt.Errorf("francetv: fetching guide: nothing on %s", ch.Name)
		}
		programmes = append(programmes, provider.Fill(ch, known, from, to, unlisted)...)
	}
	return programmes, nil
}

// listings returns what the guide has of a channel around from and to, in the
// order it is shown.
func (p *Provider) listings(ctx context.Context, ch provider.Channel, from, to time.Time) ([]entry, error) {
	// The guide answers for a channel it does not know with France 2's.
	if _, ok := find(ch.ID); !ok {
		return nil, fmt.Errorf("no channel %q", ch.ID)
	}
	// A day of the guide is a day in Paris, an hour or two ahead of UTC, and
	// runs into the next morning: hence the day before as well.
	first, last := from.UTC().Add(time.Hour).AddDate(0, 0, -1), to.UTC().Add(2*time.Hour)
	var listed []entry
	for day := first; day.Format(time.DateOnly) <= last.Format(time.DateOnly); day = day.AddDate(0, 0, 1) {
		var entries []entry
		query := url.Values{"date": {day.Format(time.DateOnly)}, "channel": {ch.ID}}
		if err := p.ask(ctx, ch.Name+"'s guide", p.guide+"?"+query.Encode(), &entries); err != nil {
			return nil, err
		}
		listed = append(listed, entries...)
	}
	slices.SortStableFunc(listed, func(a, b entry) int { return a.Content.Start.Compare(b.Content.Start) })
	return listed, nil
}

// learn looks up the programmes of the listings that are not known yet, a few
// at a time, and remembers them: a programme's name does not change.
//
// The apps' API keeps a programme at a path, which the guide does not give.
// So learn asks what is at every path the programme may be at, until the
// answer is the programme with the id the guide has for it: see paths. Some
// programmes are at none of them.
func (p *Provider) learn(ctx context.Context, channels []provider.Channel, listed [][]entry) error {
	// One guide at a time: another that is asked for meanwhile waits for the
	// names this one is looking up, rather than going without them.
	p.learning.Lock()
	defer p.learning.Unlock()

	wanted := map[number][]string{} // where to ask for each programme, by its id
	p.mu.Lock()
	for i, entries := range listed {
		for _, e := range entries {
			if _, known := p.shows[e.Tracking.ProgramID]; known || e.Tracking.ProgramID == "" {
				continue
			}
			for _, path := range e.paths(channels[i].ID) {
				if !p.asked[path] && !slices.Contains(wanted[e.Tracking.ProgramID], path) {
					wanted[e.Tracking.ProgramID] = append(wanted[e.Tracking.ProgramID], path)
				}
			}
		}
	}
	p.mu.Unlock()

	var (
		looking sync.WaitGroup
		free    = make(chan struct{}, lookups)
		failure error
	)
	for programme, paths := range wanted {
		free <- struct{}{}
		looking.Go(func() {
			defer func() { <-free }()
			for _, path := range paths {
				// Another programme may be looked for at the same path, and
				// what is there is remembered whoever asks.
				p.mu.Lock()
				_, known := p.shows[programme]
				taken := p.asked[path]
				if !known {
					p.asked[path] = true
				}
				p.mu.Unlock()
				if known {
					return
				}
				if taken {
					continue
				}
				id, there, err := p.lookUp(ctx, path)
				p.mu.Lock()
				switch {
				case err != nil:
					delete(p.asked, path)
					failure = cmp.Or(failure, err)
				case id != "":
					p.shows[id] = there
				}
				p.mu.Unlock()
				if err != nil {
					return
				}
			}
		})
	}
	looking.Wait()
	return failure
}

// lookUp asks the apps' API what is at a path: a programme, a season of one,
// a kind of programme, or nothing.
func (p *Provider) lookUp(ctx context.Context, path string) (id number, there show, err error) {
	var answer struct {
		ID     number
		Label  string
		Images []struct {
			Type string
			URLs map[string]string
		}
	}
	body, status, header, err := p.get(ctx, http.MethodGet, p.apps+"/taxonomy/"+url.PathEscape(path)+"?platform=apps")
	switch {
	case err != nil:
		return "", show{}, fmt.Errorf("looking a programme up: %w", err)
	case status == http.StatusNotFound:
		return "", show{}, nil
	case status != http.StatusOK:
		return "", show{}, refusal("the name of a programme", status, header)
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", show{}, fmt.Errorf("looking a programme up: %w", err)
	}
	there.name = strings.TrimSpace(answer.Label)
	for _, image := range answer.Images {
		if image.Type == "vignette_3x4" { // upright, like the posters Plex shows
			there.poster = cmp.Or(image.URLs["w:800"], image.URLs["w:1024"], image.URLs["w:400"])
		}
	}
	return answer.ID, there, nil
}

// schedule makes a channel's programmes between from and to of its listings.
//
// The guide gives a programme a start and a length, which do not add up to
// the start of the next: lengths are those of the recordings, or of the slots
// in the schedule, and what comes between programmes is not listed. So a
// programme ends where the next one starts, unless that is more than a lull
// after its own end.
func schedule(ch provider.Channel, listed []entry, shows map[number]show, from, to time.Time) []provider.Programme {
	var (
		programmes []provider.Programme
		last       number    // the entry before
		end        time.Time // when it ends by its own length
	)
	for _, e := range listed {
		start := e.Content.Start
		// The guide lists some programmes twice, a minute or two apart.
		if start.IsZero() || (e.Content.ID == last && start.Before(end)) {
			continue
		}
		if n := len(programmes); n > 0 && start.Sub(programmes[n-1].Stop) <= lull {
			programmes[n-1].Stop = start
		}
		last, end = e.Content.ID, start.Add(e.length())
		programme := provider.Programme{ChannelID: ch.ID, Start: start, Stop: end}
		programme.Title, programme.Description, programme.Image = describe(e, shows)
		programmes = append(programmes, programme)
	}
	return slices.DeleteFunc(programmes, func(programme provider.Programme) bool {
		return programme.Title == "" || !programme.Stop.After(programme.Start) ||
			!programme.Stop.After(from) || !programme.Start.Before(to)
	})
}

// describe returns what the guide is to say of an entry: the name of its
// programme, with the episode's on the first line of the description, and
// the programme's poster. An entry that is no episode of a programme, such as
// a film, has its own name and picture.
func describe(e entry, shows map[number]show) (title, description, image string) {
	episode := strings.TrimSpace(e.Content.Title)
	image = cmp.Or(e.Content.Images.Expanded.X1, e.Content.Images.Base.X2, e.Content.Images.Base.X1)
	programme := shows[e.Tracking.ProgramID]
	name := cmp.Or(programme.name, spelled(e.Tracking.Program))
	if name == "" {
		return episode, e.Content.Description, image
	}
	if strings.EqualFold(episode, name) {
		episode = ""
	}
	return name, strings.TrimSpace(episode + "\n" + e.Content.Description), cmp.Or(programme.poster, image)
}

// spelled makes a name of what the guide calls a programme for the
// statistics, such as "dans_le_retro": the best there is for a programme that
// the apps' API has at none of the paths asked.
func spelled(slug string) string {
	slug, _, _ = strings.Cut(slug, "|")
	words := strings.ReplaceAll(slug, "_", " ")
	first, size := utf8.DecodeRuneInString(words)
	if size == 0 {
		return ""
	}
	return string(unicode.ToUpper(first)) + words[size:]
}

// Stream asks France Télévisions where the channel's stream is and for a pass
// to it, and returns the stream once a look finds it playing here. A channel
// that does not play is told apart by what would help: see refusal.
func (p *Provider) Stream(ctx context.Context, channelID string) (provider.Source, error) {
	ch, ok := find(channelID)
	if !ok {
		return provider.Source{}, fmt.Errorf("francetv: no channel %q", channelID)
	}
	source, err := p.stream(ctx, ch)
	if err != nil {
		return provider.Source{}, fmt.Errorf("francetv: %w", err)
	}
	return source, nil
}

func (p *Provider) stream(ctx context.Context, ch channel) (provider.Source, error) {
	handed, err := p.video(ctx, ch)
	if err != nil {
		return provider.Source{}, err
	}
	signed, err := p.sign(ctx, handed)
	if err != nil {
		return provider.Source{}, fmt.Errorf("getting a pass to %s: %w", ch.name, err)
	}
	unsigned, err := url.Parse(handed.URL)
	if err != nil {
		return provider.Source{}, fmt.Errorf("%s is unavailable: the stream's address cannot be read", ch.name)
	}
	at, err := url.Parse(signed)
	if err != nil {
		return provider.Source{}, fmt.Errorf("%s is unavailable: the stream's address cannot be read", ch.name)
	}
	master, err := p.playlist(ctx, ch, at)
	if err != nil {
		return provider.Source{}, err
	}

	// Whether the stream is encrypted, has stopped or is kept from this
	// address shows in its one quality: the playlists are handed out
	// anywhere, the picture and the sound are not.
	quality := provider.BestQuality(master)
	media, within := master, at
	if quality != "" {
		if within, err = at.Parse(quality); err != nil {
			return provider.Source{}, fmt.Errorf("%s is unavailable: %w", ch.name, err)
		}
		if media, err = p.playlist(ctx, ch, within); err != nil {
			return provider.Source{}, err
		}
	}
	newest := ""
	for line := range strings.Lines(media) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			newest = line
		}
	}
	switch {
	// AES-128 is no obstacle: its key is there for ffmpeg to fetch.
	case strings.Contains(master+media, "METHOD=SAMPLE-AES"):
		return provider.Source{}, fmt.Errorf("%s is DRM-protected", ch.name)
	case !strings.Contains(media, "#EXTINF") || newest == "":
		return provider.Source{}, fmt.Errorf("%s is unavailable: its playlist is empty", ch.name)
	}
	segment, err := within.Parse(newest)
	if err != nil {
		return provider.Source{}, fmt.Errorf("%s is unavailable: %w", ch.name, err)
	}
	if _, status, header, err := p.get(ctx, http.MethodHead, segment.String()); err != nil {
		return provider.Source{}, fmt.Errorf("reaching %s: %w", ch.name, err)
	} else if status != http.StatusOK {
		return provider.Source{}, refusal(ch.name, status, header)
	}

	pass := passOf(at, unsigned)
	s := &session{
		renewed: func(ctx context.Context) string {
			if signed, err := p.sign(ctx, handed); err == nil {
				if again, err := url.Parse(signed); err == nil {
					return passOf(again, unsigned)
				}
			}
			return ""
		},
		rest:   p.rest,
		first:  pass,
		pass:   pass,
		signed: time.Now(),
	}
	client := *p.client
	client.Transport = httpclient.Wrap(p.client.Transport, s.roundTrip)
	return provider.Source{URL: signed, Client: &client}, nil
}

// feed is a channel's stream as the player's API hands it out.
type feed struct {
	URL   string // its master playlist, which is refused without a pass
	Token struct {
		Akamai string // where to get a pass: see sign
	}
	DRM    bool
	Format string
}

// video asks the player's API for the channel's live video.
//
// The video's id comes from the apps' API, which lists what is on every
// channel now. The ids are remembered, as they stay the same, until the
// player's API will not have one of them.
func (p *Provider) video(ctx context.Context, ch channel) (feed, error) {
	p.mu.Lock()
	id := p.live[ch.id]
	p.mu.Unlock()
	if id == "" {
		var directs struct {
			Items []struct {
				Channel struct {
					Path string `json:"channel_path"`
					Live string `json:"si_id"`
				}
			}
		}
		if err := p.ask(ctx, "the list of live channels", p.apps+"/directs?platform=apps", &directs); err != nil {
			return feed{}, err
		}
		p.mu.Lock()
		for _, item := range directs.Items {
			if item.Channel.Path != "" && item.Channel.Live != "" {
				p.live[item.Channel.Path] = item.Channel.Live
			}
		}
		id = p.live[ch.id]
		p.mu.Unlock()
		if id == "" {
			return feed{}, fmt.Errorf("%s is unavailable: France Télévisions lists no live stream of it", ch.name)
		}
	}

	// The API refuses requests that do not say what plays the video, and
	// where.
	query := url.Values{"device_type": {"desktop"}, "browser": {"chrome"}, "domain": {"www.france.tv"}}
	var answer struct {
		Video   feed
		Message string // of a refusal, in French
	}
	body, status, _, err := p.get(ctx, http.MethodGet, p.player+"/"+url.PathEscape(id)+"?"+query.Encode())
	if err != nil {
		return feed{}, fmt.Errorf("resolving stream: %w", err)
	}
	json.Unmarshal(body, &answer) // a refusal may come without a message
	switch {
	case status != http.StatusOK:
		p.mu.Lock()
		delete(p.live, ch.id)
		p.mu.Unlock()
		return feed{}, fmt.Errorf("%s is unavailable: France Télévisions answers %s",
			ch.name, strings.TrimSpace(strconv.Itoa(status)+" "+answer.Message))
	case answer.Video.DRM:
		return feed{}, fmt.Errorf("%s is DRM-protected", ch.name)
	case answer.Video.Format != "hls" || answer.Video.URL == "":
		return feed{}, fmt.Errorf("%s has no HLS stream", ch.name)
	}
	return answer.Video, nil
}

// sign returns the address of a stream's master playlist with a pass to the
// stream in it.
func (p *Provider) sign(ctx context.Context, stream feed) (string, error) {
	if stream.Token.Akamai == "" {
		return stream.URL, nil // none is asked for
	}
	signer, err := url.Parse(stream.Token.Akamai)
	if err != nil {
		return "", errors.New("the address to ask at cannot be read")
	}
	query := signer.Query()
	query.Set("format", "json")
	query.Set("url", stream.URL)
	signer.RawQuery = query.Encode()
	var answer struct{ URL string }
	if err := p.ask(ctx, "the pass", signer.String(), &answer); err != nil {
		return "", err
	}
	if answer.URL == "" {
		return "", errors.New("France Télévisions hands none out")
	}
	return answer.URL, nil
}

// passOf returns the pass in the signed address of a stream, as it stands in
// front of the path: /pass in /pass/live/index.m3u8. It is empty for an
// address that is signed in another way.
func passOf(signed, unsigned *url.URL) string {
	pass, ok := strings.CutSuffix(signed.Path, unsigned.Path)
	if !ok || signed.Host != unsigned.Host || !strings.HasPrefix(pass, "/") {
		return ""
	}
	return pass
}

// playlist fetches an HLS playlist of a channel.
func (p *Provider) playlist(ctx context.Context, ch channel, address *url.URL) (string, error) {
	body, status, header, err := p.get(ctx, http.MethodGet, address.String())
	switch {
	case err != nil:
		return "", fmt.Errorf("reaching %s: %w", ch.name, err)
	case status != http.StatusOK:
		return "", refusal(ch.name, status, header)
	case !strings.HasPrefix(string(body), "#EXTM3U"):
		return "", fmt.Errorf("%s is unavailable: France Télévisions sends no playlist", ch.name)
	}
	return string(body), nil
}

// session sends the requests of a stream's HTTP client, which the stream is
// read through, and keeps the stream's pass good.
//
// The pass is in the path of every address of the stream, and lasts six
// hours. The stream goes on being asked for with the one it started with, so
// a request that is refused is sent again with a new pass, which the requests
// after it then go with.
type session struct {
	renewed func(context.Context) string // gets a new pass, or none
	rest    time.Duration                // how long a pass is left alone: one this new is not refused for its age

	mu     sync.Mutex
	first  string    // the pass in the addresses ffmpeg asks for; empty if they carry none
	pass   string    // the pass to ask with
	signed time.Time // when it was handed out
}

func (s *session) roundTrip(req *http.Request, transport http.RoundTripper) (*http.Response, error) {
	isPlaylist := strings.HasSuffix(req.URL.Path, ".m3u8")
	send := func(pass string) (*http.Response, error) {
		out := req.Clone(req.Context())
		// A playlist holds the last four hours, and compressed it is a
		// fortieth of the size. Go asks for that by itself, but not of a
		// range, and ffmpeg asks for a range of everything.
		if isPlaylist {
			out.Header.Del("Range")
		}
		if file, ok := strings.CutPrefix(out.URL.Path, s.first+"/"); ok && s.first != "" {
			out.URL.Path, out.URL.RawPath = pass+"/"+file, ""
		}
		resp, err := transport.RoundTrip(out)
		if resp != nil {
			// The answer is to what was asked. What a playlist lists is
			// then asked for the same way, with the pass that is renewed.
			resp.Request = req
		}
		return resp, err
	}
	s.mu.Lock()
	pass := s.pass
	s.mu.Unlock()
	resp, err := send(pass)
	if err == nil && resp.StatusCode == http.StatusForbidden {
		if pass = s.renew(req.Context(), pass); pass != "" {
			resp.Body.Close()
			resp, err = send(pass)
		}
	}
	if err != nil || !isPlaylist || resp.StatusCode != http.StatusOK {
		return resp, err
	}

	// Some playlists name the key to their stream by its full address, which
	// ffmpeg would ask for itself, past the proxy, and be refused. Named by
	// its path, the key is asked for here like the rest.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	playlist := strings.ReplaceAll(string(body), req.URL.Scheme+"://"+req.URL.Host+"/", "/")
	resp.Body = io.NopCloser(strings.NewReader(playlist))
	resp.ContentLength = int64(len(playlist))
	resp.Header.Set("Content-Length", strconv.Itoa(len(playlist)))
	return resp, nil
}

// renew returns the pass to ask with after one was refused: a new one, or
// none if a new one will not help.
func (s *session) renew(ctx context.Context, refused string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.first == "":
		return ""
	case s.pass != refused:
		return s.pass // another request has renewed it since
	case time.Since(s.signed) < s.rest:
		return ""
	}
	pass := s.renewed(ctx)
	if pass == "" {
		return ""
	}
	s.pass, s.signed = pass, time.Now()
	return pass
}

// find looks a channel up by its id.
func find(id string) (channel, bool) {
	i := slices.IndexFunc(channels, func(ch channel) bool { return ch.id == id })
	if i < 0 {
		return channel{}, false
	}
	return channels[i], true
}

// refusal puts into words that France Télévisions did not hand something
// out, by what would help. Its CDN gives its reason for a 403 in a header:
// geo for an address outside France. Anything but a refusal is a stream or a
// guide that is not there, which neither an account nor a proxy brings back.
func refusal(what string, status int, header http.Header) error {
	reason := header.Get("X-ErrorType")
	switch {
	case status == http.StatusUnavailableForLegalReasons, status == http.StatusForbidden && reason == "geo":
		return fmt.Errorf("%s is blocked outside France: set -francetv-proxy to a proxy with a French exit, or try another exit", what)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s asks for a sign-in, which telesfor has none for", what)
	case status == http.StatusForbidden:
		return fmt.Errorf("%s is refused here: France Télévisions answers %s", what, strings.TrimSpace("403 Forbidden "+reason))
	}
	return fmt.Errorf("%s is unavailable: France Télévisions answers %d %s", what, status, http.StatusText(status))
}

// ask fetches an address of one of the APIs and decodes its JSON answer into
// v.
func (p *Provider) ask(ctx context.Context, what, address string, v any) error {
	body, status, header, err := p.get(ctx, http.MethodGet, address)
	switch {
	case err != nil:
		return fmt.Errorf("reaching %s: %w", what, err)
	case status != http.StatusOK:
		return refusal(what, status, header)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("reading %s: %w", what, err)
	}
	return nil
}

// get sends a request to France Télévisions and returns the answer with its
// status and headers.
func (p *Provider) get(ctx context.Context, method, address string) (body []byte, status int, header http.Header, err error) {
	req, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, nil, errors.Unwrap(err) // without the address, which may carry a stream's pass
	}
	defer resp.Body.Close()

	// A playlist of four hours is the longest of them, at under a megabyte.
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, nil, err
	}
	return body, resp.StatusCode, resp.Header, nil
}
