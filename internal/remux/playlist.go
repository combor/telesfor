package remux

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// playlist is the playlist of a quality or of the sound: the segments it
// lists now.
type playlist struct {
	version  int
	target   time.Duration // the longest a segment gets
	segments []entry
	ended    bool // no more are to come
}

// entry is a segment of a stream, as its playlist lists it.
type entry struct {
	seq    int64         // its number in the stream
	uri    *url.URL      // where it is
	length time.Duration // how long it plays
	extinf string        // the same, as the playlist puts it
	at     time.Time     // when it was aired, where the playlist tells
	key    string        // the attributes of the key it is encrypted with, or ""
	init   string        // the attributes of the section that a player reads before it, or ""
	bytes  string        // the part of the file that it is, as length@start, or "" for all of it
	breaks bool          // it does not follow on from the one before
}

// first is the number of the first segment listed, and next that of the one to
// come after the last.
func (p playlist) first() int64 {
	if len(p.segments) == 0 {
		return 0
	}
	return p.segments[0].seq
}

func (p playlist) next() int64 { return p.first() + int64(len(p.segments)) }

// find looks a segment up by its number.
func (p playlist) find(seq int64) (entry, bool) {
	if i := seq - p.first(); i >= 0 && i < int64(len(p.segments)) {
		return p.segments[i], true
	}
	return entry{}, false
}

// dateTimes are the ways playlists write the time a segment was aired at.
var dateTimes = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700"}

// parsePlaylist reads the playlist of segments found at an address. The
// addresses in it are made whole.
func parsePlaylist(body string, at *url.URL) (playlist, error) {
	var p playlist
	if !strings.HasPrefix(body, "#EXTM3U") {
		return p, errors.New("not a playlist")
	}
	whole := func(list string) string { // the attributes of a tag, with its URI made whole
		uri, ok := attributes(list)["URI"]
		if address, err := at.Parse(uri); ok && err == nil {
			return strings.Replace(list, `URI="`+uri+`"`, `URI="`+address.String()+`"`, 1)
		}
		return list
	}

	seq := int64(0)
	var next entry             // what is known of the segment to come
	var key, init string       // in force until the playlist says otherwise
	var aired time.Time        // when the segment to come was, where known
	ends := map[string]int64{} // where the last part taken of each file ended
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		tag, value, _ := strings.Cut(line, ":")
		switch {
		case line == "":
		case tag == "#EXT-X-VERSION":
			p.version, _ = strconv.Atoi(value)
		case tag == "#EXT-X-TARGETDURATION":
			seconds, _ := strconv.Atoi(value)
			p.target = time.Duration(seconds) * time.Second
		case tag == "#EXT-X-MEDIA-SEQUENCE":
			seq, _ = strconv.ParseInt(value, 10, 64)
		case tag == "#EXT-X-KEY":
			if key = whole(value); attributes(value)["METHOD"] == "NONE" {
				key = ""
			}
		case tag == "#EXT-X-MAP":
			init = whole(value)
		case tag == "#EXT-X-PROGRAM-DATE-TIME":
			for _, layout := range dateTimes {
				if t, err := time.Parse(layout, value); err == nil {
					aired = t
					break
				}
			}
		case tag == "#EXT-X-DISCONTINUITY":
			next.breaks = true
		case tag == "#EXT-X-BYTERANGE":
			next.bytes = value
		case tag == "#EXTINF":
			next.extinf, _, _ = strings.Cut(value, ",")
			seconds, _ := strconv.ParseFloat(next.extinf, 64)
			next.length = time.Duration(seconds * float64(time.Second))
		case tag == "#EXT-X-ENDLIST":
			p.ended = true
		case strings.HasPrefix(line, "#"):
		default:
			uri, err := at.Parse(line)
			if err != nil {
				return p, fmt.Errorf("segment %d: %w", seq, errors.Unwrap(err))
			}
			next.seq, next.uri, next.key, next.init, next.at = seq, uri, key, init, aired
			// A part that names no start begins where the one before ended.
			if length, start, ok := strings.Cut(next.bytes, "@"); next.bytes != "" {
				n, _ := strconv.ParseInt(length, 10, 64)
				from := ends[uri.String()]
				if ok {
					from, _ = strconv.ParseInt(start, 10, 64)
				}
				next.bytes, ends[uri.String()] = fmt.Sprintf("%d@%d", n, from), from+n
			}
			p.segments = append(p.segments, next)
			p.target = max(p.target, next.length) // whatever the playlist says
			if !aired.IsZero() {
				aired = aired.Add(next.length)
			}
			seq, next = seq+1, entry{}
		}
	}
	return p, nil
}

// write renders the playlist for ffmpeg: its segments from one number up to,
// but not including, another, or to the last if until is negative. Every
// address in it is replaced by what link makes of it. A playlist that is
// written as ended tells ffmpeg to stop after its last segment.
//
// The segments are numbered anew, from the one that zero names. ffmpeg takes
// the numbers of a stream's playlists to mean the same, and where one playlist
// is a segment behind another, it skips that segment to catch up. A leg's
// picture and sound begin where they begin, so both are numbered from the
// segment they begin with. A key that is told apart by the number of its
// segment, as a key is that names nothing else, is given that number to go
// by.
func (p playlist) write(from, until, zero int64, ended bool, link func(kind string, seq int64, address string) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:%d\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n",
		max(p.version, 3), int(math.Ceil(p.target.Seconds())), max(max(from, p.first())-zero, 0))
	linked := func(kind string, seq int64, list string) string {
		uri := attributes(list)["URI"]
		return strings.Replace(list, `URI="`+uri+`"`, `URI="`+link(kind, seq, uri)+`"`, 1)
	}
	var key, init string
	listed := false
	for _, s := range p.segments {
		if s.seq < from || (until >= 0 && s.seq >= until) {
			continue
		}
		if s.breaks && listed {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		uses := s.key
		if uses != "" && attributes(uses)["IV"] == "" {
			uses += fmt.Sprintf(",IV=0x%032X", s.seq)
		}
		if uses != key {
			if key = uses; key == "" {
				b.WriteString("#EXT-X-KEY:METHOD=NONE\n")
			} else {
				b.WriteString("#EXT-X-KEY:" + linked("key", s.seq, key) + "\n")
			}
		}
		if s.init != init && s.init != "" {
			init = s.init
			b.WriteString("#EXT-X-MAP:" + linked("init", s.seq, init) + "\n")
		}
		b.WriteString("#EXTINF:" + s.extinf + ",\n")
		if s.bytes != "" {
			b.WriteString("#EXT-X-BYTERANGE:" + s.bytes + "\n")
		}
		b.WriteString(link("segment", s.seq, s.uri.String()) + "\n")
		listed = true
	}
	if ended || p.ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

// starting returns the number of the segment that starts at a time, or the
// nearest to it, whether the playlist lists it yet, or still, or not. It fails
// for a playlist that does not tell when its segments were aired.
func (p playlist) starting(at time.Time) (int64, bool) {
	if len(p.segments) == 0 || p.segments[0].at.IsZero() || at.IsZero() {
		return 0, false
	}
	for _, s := range p.segments {
		if s.at.Sub(at).Abs() <= s.length/2 {
			return s.seq, true
		}
	}
	// It is as many segments from the nearer end as it is seconds.
	near := p.segments[len(p.segments)-1]
	if at.Before(p.segments[0].at) {
		near = p.segments[0]
	}
	if near.length <= 0 {
		return 0, false
	}
	return near.seq + int64(math.Round(float64(at.Sub(near.at))/float64(near.length))), true
}

// ahead tells how far the numbers of one playlist are ahead of another's of
// the same stream: what to add to the number of a segment in p for the one in
// other that plays at the same time. Qualities of a stream are cut at the
// same places, and nearly always numbered alike, but nothing says they must
// be. It fails where the two cannot be lined up.
func (p playlist) ahead(other playlist) (int64, bool) {
	if len(p.segments) == 0 || len(other.segments) == 0 {
		return 0, false
	}
	// By the clock, where both playlists tell the time.
	last := p.segments[len(p.segments)-1]
	if same, ok := other.starting(last.at); ok {
		return same - last.seq, true
	}
	// By the numbers, unless the segments they share differ in length.
	for _, s := range p.segments {
		if o, ok := other.find(s.seq); ok && (s.length-o.length).Abs() > 10*time.Millisecond {
			return 0, false
		}
	}
	// A segment may be a little longer than the playlist says they get.
	return 0, p.target.Round(time.Second) == other.target.Round(time.Second)
}
