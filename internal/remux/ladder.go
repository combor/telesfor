package remux

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ladder is a stream that comes in more than one quality: what its master
// playlist offers of them.
type ladder struct {
	qualities []quality // from the lowest rate to the highest
	sound     *url.URL  // the playlist of the sound, where it is apart from the picture
}

// quality is one of the pictures a stream comes in.
type quality struct {
	playlist *url.URL // of its segments
	rate     int      // the most bits a second it takes: its BANDWIDTH
	average  int      // what it takes on average, where the playlist tells
	height   int      // of the picture, in lines; 0 where the playlist does not tell
}

// String names a quality for the log.
func (q quality) String() string {
	if q.height == 0 {
		return megabits(float64(q.rate))
	}
	return strconv.Itoa(q.height) + "p"
}

// megabits formats a number of bits a second for the log.
func megabits(rate float64) string { return fmt.Sprintf("%.1fMbps", rate/1e6) }

// attributes reads the attributes of a playlist tag: NAME=value between
// commas, where a value in quotes may hold commas of its own.
func attributes(list string) map[string]string {
	attributes := map[string]string{}
	for list != "" {
		name, rest, _ := strings.Cut(list, "=")
		var value string
		if quoted, ok := strings.CutPrefix(rest, `"`); ok {
			value, rest, _ = strings.Cut(quoted, `"`)
			_, rest, _ = strings.Cut(rest, ",")
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		attributes[strings.TrimSpace(name)] = strings.TrimSpace(value)
		list = rest
	}
	return attributes
}

// picture names the kind of video in a list of codecs, such as "avc" for
// H.264 under either of its names, or "" if there is none in it.
func picture(codecs string) string {
	for codec := range strings.SplitSeq(codecs, ",") {
		kind, _, _ := strings.Cut(strings.TrimSpace(codec), ".")
		switch kind {
		case "avc1", "avc3":
			return "avc"
		case "hvc1", "hev1":
			return "hevc"
		case "", "mp4a", "ac-3", "ec-3", "opus", "fLaC", "stpp", "wvtt":
		default:
			return kind
		}
	}
	return ""
}

// parseMaster reads the qualities out of the master playlist found at an
// address. It returns nil for a playlist with fewer than two that ffmpeg can
// go from one to the other of: there is nothing to choose between then.
//
// The qualities kept are those with the same kind of video and the same
// sound as the best. The sound is the default of its group.
func parseMaster(body string, at *url.URL) *ladder {
	type variant struct {
		quality
		picture, sound string
		seen           bool // whether the playlist says what is in it
	}
	var variants []variant
	sounds := map[string]string{} // the URI of each group's sound, or "" where it comes with the picture
	chosen := map[string]bool{}   // the groups whose default has been found
	var next *variant
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if list, ok := strings.CutPrefix(line, "#EXT-X-STREAM-INF:"); ok {
			a := attributes(list)
			v := variant{picture: picture(a["CODECS"]), sound: a["AUDIO"], seen: a["CODECS"] != ""}
			v.rate, _ = strconv.Atoi(a["BANDWIDTH"])
			v.average, _ = strconv.Atoi(a["AVERAGE-BANDWIDTH"])
			if _, height, ok := strings.Cut(a["RESOLUTION"], "x"); ok {
				v.height, _ = strconv.Atoi(height)
			}
			next = &v
		} else if list, ok := strings.CutPrefix(line, "#EXT-X-MEDIA:"); ok {
			a := attributes(list)
			if group := a["GROUP-ID"]; a["TYPE"] == "AUDIO" && !chosen[group] {
				if _, known := sounds[group]; !known || a["DEFAULT"] == "YES" {
					sounds[group], chosen[group] = a["URI"], a["DEFAULT"] == "YES"
				}
			}
		} else if line != "" && !strings.HasPrefix(line, "#") && next != nil {
			if playlist, err := at.Parse(line); err == nil {
				next.playlist = playlist
				variants = append(variants, *next)
			}
			next = nil
		}
	}

	// Sound alone is listed like a quality, but for its codecs.
	variants = slices.DeleteFunc(variants, func(v variant) bool {
		return v.rate <= 0 || (v.seen && v.picture == "" && v.height == 0)
	})
	if len(variants) < 2 {
		return nil
	}
	best := slices.MaxFunc(variants, func(a, b variant) int { return a.rate - b.rate })
	l := &ladder{}
	for _, v := range variants {
		same := func(q quality) bool { return q.playlist.String() == v.playlist.String() }
		if v.picture == best.picture && v.sound == best.sound && !slices.ContainsFunc(l.qualities, same) {
			l.qualities = append(l.qualities, v.quality)
		}
	}
	if len(l.qualities) < 2 {
		return nil
	}
	slices.SortStableFunc(l.qualities, func(a, b quality) int { return a.rate - b.rate })
	if uri := sounds[best.sound]; uri != "" {
		sound, err := at.Parse(uri)
		if err != nil {
			return nil
		}
		l.sound = sound
	}
	return l
}
