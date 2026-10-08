package tf1

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Paris time, where the system has no zones: see paris

	"github.com/combor/telesfor/internal/provider"
)

const (
	// block is how long a placeholder lasts in the guide.
	block = time.Hour

	// unlisted is what a placeholder says of itself.
	unlisted = "TF1 has published no listings for this time."
)

// paris is the time TF1's guide is laid out in.
var paris = func() *time.Location {
	zone, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err) // the zones are built in
	}
	return zone
}()

// Programmes fetches the guide of the channels that TF1 publishes one of. A
// channel without one has its name on every hour: Plex offers a channel by
// what is on it, so a channel with nothing on has nothing to pick there.
//
// It is also when telesfor looks after the account: its tokens are renewed
// by use, and the tuner fetches the guide several times a day.
func (p *Provider) Programmes(ctx context.Context, listed []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	p.token(ctx, "")

	known := make([][]provider.Programme, len(listed))
	failed := make([]error, len(listed))
	var fetching sync.WaitGroup
	for i, l := range listed {
		if ch, ok := find(l.ID); ok && ch.guide != "" {
			fetching.Go(func() {
				// As when TF1 lays its guide out anew. Placeholders alone
				// would wipe what Plex has of the channel.
				if known[i], failed[i] = p.listings(ctx, ch, from, to); failed[i] == nil && len(known[i]) == 0 {
					failed[i] = fmt.Errorf("nothing on %s", ch.name)
				}
			})
		}
	}
	fetching.Wait()

	var programmes []provider.Programme
	for i, l := range listed {
		if failed[i] != nil {
			return nil, fmt.Errorf("tf1: fetching guide: %w", failed[i])
		}
		programmes = append(programmes, fill(l, known[i], from, to)...)
	}
	return programmes, nil
}

// listings returns what the guide has of a channel between from and to.
//
// The guide gives a programme a start and no end. So a programme runs until
// the next one starts, which for the last of a day is the first of the next.
// That takes the day before those asked for: see broadcast.
func (p *Provider) listings(ctx context.Context, ch channel, from, to time.Time) ([]provider.Programme, error) {
	var listed []provider.Programme
	year, month, day := from.In(paris).Date()
	first, last := time.Date(year, month, day-1, 0, 0, 0, 0, paris), to.In(paris)
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		address := p.guide + "/" + url.PathEscape(ch.guide) + "/jour/" + day.Format(time.DateOnly)
		page, status, err := p.page(ctx, address)
		switch {
		case err != nil:
			return nil, fmt.Errorf("reaching %s's guide: %w", ch.name, err)
		case status != http.StatusOK:
			return nil, fmt.Errorf("%s's guide is unavailable: TF1 answers %d %s", ch.name, status, http.StatusText(status))
		}
		listed = append(listed, broadcast(page, day, address)...)
	}

	var programmes []provider.Programme
	for i := 0; i < len(listed)-1; i++ {
		programme := listed[i]
		programme.ChannelID, programme.Stop = ch.id, listed[i+1].Start
		// A day or more until the next programme is a day the guide lacks:
		// when this one ends is not known.
		if programme.Title == "" || !programme.Stop.After(programme.Start) || programme.Stop.Sub(programme.Start) >= 24*time.Hour ||
			!programme.Stop.After(from) || !programme.Start.Before(to) {
			continue
		}
		programmes = append(programmes, programme)
	}
	return programmes, nil
}

// What a day of the guide has of a programme: the time it starts at, its
// name, the episode's, what it is about and a picture. The episode's name is
// mostly left out.
var (
	clock   = regexp.MustCompile(`class="heure">\s*(\d\d):(\d\d)`)
	name    = regexp.MustCompile(`(?s)<h5[^>]*>(.*?)</h5>`)
	episode = regexp.MustCompile(`(?s)class="episode">(.*?)</div>`)
	about   = regexp.MustCompile(`(?s)class="resume">(.*?)</div>`)
	picture = regexp.MustCompile(`<source srcset="([^" ]+)`)
	tag     = regexp.MustCompile(`<[^>]*>`)
)

// broadcast reads a day of the guide, found at an address. It is a day of the
// broadcast: it starts at about six in the morning and runs into the next,
// so the times that turn back to the small hours are of the day after. The
// programmes it returns have no end yet.
func broadcast(page string, day time.Time, address string) []provider.Programme {
	at, _ := url.Parse(address)
	year, month, date := day.Date()
	var programmes []provider.Programme
	last := -1 // the time of the row before, in minutes of the day
	for _, row := range strings.Split(page, `class="views-row`)[1:] {
		starts := clock.FindStringSubmatch(row)
		if starts == nil {
			continue
		}
		hours, _ := strconv.Atoi(starts[1])
		minutes, _ := strconv.Atoi(starts[2])
		// The day after is told by the time alone, and the time is then
		// made on that day: on the night the clocks go forward, a time of
		// the day after may be none of the day of the page.
		if hours*60+minutes < last {
			date++
		}
		last = hours*60 + minutes
		programme := provider.Programme{Start: time.Date(year, month, date, hours, minutes, 0, 0, paris)}
		if title := name.FindStringSubmatch(row); title != nil {
			programme.Title = text(title[1])
		}
		for _, part := range []*regexp.Regexp{episode, about} {
			if found := part.FindStringSubmatch(row); found != nil {
				programme.Description = strings.TrimSpace(programme.Description + "\n" + text(found[1]))
			}
		}
		if src := picture.FindStringSubmatch(row); src != nil && at != nil {
			if image, err := at.Parse(html.UnescapeString(src[1])); err == nil {
				programme.Image = image.String()
			}
		}
		programmes = append(programmes, programme)
	}
	return programmes
}

// text returns the words of a piece of HTML, a paragraph to a line.
func text(markup string) string {
	var lines []string
	for line := range strings.Lines(html.UnescapeString(tag.ReplaceAllString(markup, ""))) {
		if words := strings.Join(strings.Fields(line), " "); words != "" {
			lines = append(lines, words)
		}
	}
	return strings.Join(lines, "\n")
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

// page fetches a page of the guide.
func (p *Provider) page(ctx context.Context, address string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	// A day of a channel is under 200 kB.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", 0, err
	}
	return string(body), resp.StatusCode, nil
}
