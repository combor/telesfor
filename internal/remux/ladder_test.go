package remux

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, address string) *url.URL {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestParseMaster(t *testing.T) {
	at := mustParse(t, "https://cdn.example/live/token/master.m3u8")
	// The shape of France Télévisions': the lowest quality first, sounds of
	// which the second is the default, subtitles, and what players use to
	// wind through a stream. And what theirs does not have: a quality in
	// another kind of video, which ffmpeg could not go on in, and the sound
	// alone.
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,URI="../sound/original.m3u8",GROUP-ID="audio",NAME="Version Originale",AUTOSELECT=YES
#EXT-X-MEDIA:TYPE=AUDIO,URI="../sound/fra.m3u8",GROUP-ID="audio",NAME="Francais, bien sûr",DEFAULT=YES
#EXT-X-MEDIA:TYPE=SUBTITLES,URI="text.m3u8",GROUP-ID="text",DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=255244,AVERAGE-BANDWIDTH=232039,CODECS="avc1.42801e,mp4a.40.2",RESOLUTION=256x144,AUDIO="audio",SUBTITLES="text"
low.m3u8
#EXT-X-STREAM-INF:CODECS="avc1.640029,mp4a.40.2",AVERAGE-BANDWIDTH=5406959,RESOLUTION=1920x1080,BANDWIDTH=5947655,AUDIO="audio"
high.m3u8?part=1
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=58976,CODECS="avc1.640029",URI="frames.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1225356,CODECS="avc3.4d401e,mp4a.40.2",RESOLUTION=640x360,AUDIO="audio"
/elsewhere/middle.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,CODECS="hvc1.2.4.L123.B0,mp4a.40.2",RESOLUTION=1280x720,AUDIO="audio"
another-codec.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=96000,CODECS="mp4a.40.2",AUDIO="audio"
sound-alone.m3u8
`
	l := parseMaster(master, at)
	if l == nil {
		t.Fatal("parseMaster() found no qualities to go between")
	}
	var got []string
	for _, q := range l.qualities {
		got = append(got, q.String()+" "+megabits(float64(q.rate))+" "+megabits(float64(q.average))+" "+q.playlist.String())
	}
	want := []string{
		"144p 0.3Mbps 0.2Mbps https://cdn.example/live/token/low.m3u8",
		"360p 1.2Mbps 0.0Mbps https://cdn.example/elsewhere/middle.m3u8",
		"1080p 5.9Mbps 5.4Mbps https://cdn.example/live/token/high.m3u8?part=1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("qualities:\n%s\nwant the H.264 ones, from the lowest rate up:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if l.sound == nil || l.sound.String() != "https://cdn.example/live/sound/fra.m3u8" {
		t.Errorf("sound = %v, want the default of the group", l.sound)
	}

	// Sound that comes with the picture has no playlist of its own.
	within := strings.ReplaceAll(strings.ReplaceAll(master, `URI="../sound/original.m3u8",`, ""), `URI="../sound/fra.m3u8",`, "")
	if l := parseMaster(within, at); l == nil || l.sound != nil {
		t.Errorf("with the sound in the picture: got %+v, want qualities and no sound apart", l)
	}

	for name, master := range map[string]string{
		"one quality":        "#EXTM3U\n#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=716800,RESOLUTION=960x540\nmedia.m3u8\n",
		"a quality's own":    "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nsegment.ts\n",
		"the same one twice": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmedia.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2\nmedia.m3u8\n",
		"two kinds of video": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"avc1.640029\"\na.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2,CODECS=\"hvc1.2.4.L123.B0\"\nb.m3u8\n",
		"not a playlist":     "<html>",
	} {
		if l := parseMaster(master, at); l != nil {
			t.Errorf("%s: got %d qualities, want nothing to go between", name, len(l.qualities))
		}
	}
}

// A playlist is read, and written for ffmpeg from a segment on.
func TestPlaylist(t *testing.T) {
	at := mustParse(t, "https://cdn.example/live/token/video/playlist.m3u8")
	// Byte ranges as TVP's playlists for winding have them, keys as France
	// Télévisions' have them, one for every segment and told apart by its
	// number alone, and a break as an advert makes.
	p, err := parsePlaylist(`#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PROGRAM-DATE-TIME:2026-10-06T21:40:22.360Z
#EXT-X-KEY:METHOD=AES-128,URI="https://keys.example/key?id=1"
#EXTINF:2.00000,
#EXT-X-BYTERANGE:1000@76
one.mp4
#EXT-X-KEY:METHOD=AES-128,URI="/keys/2",IV=0x0000000000000000000000000000abcd
#EXTINF:2.50000,
#EXT-X-BYTERANGE:500
one.mp4
#EXT-X-DISCONTINUITY
#EXT-X-KEY:METHOD=NONE
#EXT-X-CUE-OUT:30
#EXTINF:1.5,advert
../other/two.mp4?token=abc
`, at)
	if err != nil {
		t.Fatal(err)
	}
	if p.first() != 100 || p.next() != 103 || p.target != 4*time.Second || p.ended {
		t.Errorf("got segments %d to %d, at most %v long, ended: %t; want 100 to 102 of 4 s, going on", p.first(), p.next()-1, p.target, p.ended)
	}
	last, _ := p.find(102)
	if want := time.Date(2026, 10, 6, 21, 40, 26, 860e6, time.UTC); !last.at.Equal(want) || last.length != 1500*time.Millisecond || !last.breaks {
		t.Errorf("the last segment is %v long, aired at %v, breaking off: %t; want 1.5 s at %v, breaking off", last.length, last.at, last.breaks, want)
	}

	link := func(kind string, seq int64, address string) string { return kind + "(" + address + ")" }
	if got, want := p.write(100, -1, 90, false, link), `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:10
#EXT-X-KEY:METHOD=AES-128,URI="key(https://keys.example/key?id=1)",IV=0x00000000000000000000000000000064
#EXT-X-MAP:URI="init(https://cdn.example/live/token/video/init.mp4)"
#EXTINF:2.00000,
#EXT-X-BYTERANGE:1000@76
segment(https://cdn.example/live/token/video/one.mp4)
#EXT-X-KEY:METHOD=AES-128,URI="key(https://cdn.example/keys/2)",IV=0x0000000000000000000000000000abcd
#EXTINF:2.50000,
#EXT-X-BYTERANGE:500@1076
segment(https://cdn.example/live/token/video/one.mp4)
#EXT-X-DISCONTINUITY
#EXT-X-KEY:METHOD=NONE
#EXTINF:1.5,
segment(https://cdn.example/live/token/other/two.mp4?token=abc)
`; got != want {
		t.Errorf("written whole:\n%s\nwant:\n%s", got, want)
	}

	// From its second segment to before its third, as a leg that is ending
	// has it: what was in force before goes with it.
	if got, want := p.write(101, 102, 101, true, link), `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-KEY:METHOD=AES-128,URI="key(https://cdn.example/keys/2)",IV=0x0000000000000000000000000000abcd
#EXT-X-MAP:URI="init(https://cdn.example/live/token/video/init.mp4)"
#EXTINF:2.50000,
#EXT-X-BYTERANGE:500@1076
segment(https://cdn.example/live/token/video/one.mp4)
#EXT-X-ENDLIST
`; got != want {
		t.Errorf("written in part:\n%s\nwant:\n%s", got, want)
	}

	if _, err := parsePlaylist("<html>", at); err == nil {
		t.Error("what is no playlist was read as one")
	}
}

// listOf builds a playlist of segments of the given lengths, numbered from
// first, which was aired on 2026-10-06 at the given time, if any.
func listOf(t *testing.T, first int, aired string, lengths ...string) playlist {
	t.Helper()
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(first) + "\n"
	if aired != "" {
		body += "#EXT-X-PROGRAM-DATE-TIME:2026-10-06T" + aired + "\n"
	}
	for _, length := range lengths {
		body += "#EXTINF:" + length + ",\nsegment.ts\n"
	}
	p, err := parsePlaylist(body, mustParse(t, "https://cdn.example/playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The segments of two playlists of a stream are lined up by when they were
// aired, and by their numbers where the playlists do not tell.
func TestPlaylistAhead(t *testing.T) {
	list := func(first int, aired string, lengths ...string) playlist {
		return listOf(t, first, aired, lengths...)
	}
	for _, test := range []struct {
		name     string
		p, other playlist
		want     int64
		fails    bool
	}{
		{"numbered alike", list(10, "21:00:00Z", "2", "2", "2"), list(10, "21:00:00.000+00:00", "2", "2", "2"), 0, false},
		{"the other a segment behind, as TVP's are at times", list(11, "21:00:02Z", "2", "2", "2"), list(10, "21:00:00Z", "2", "2", "2"), 0, false},
		{"the other numbered from elsewhere", list(10, "21:00:00Z", "2", "2", "2"), list(510, "21:00:00+0000", "2", "2", "2"), 500, false},
		{"the other long past where this one stopped", list(10, "21:00:00Z", "2", "2"), list(40, "21:01:00Z", "2", "2"), 0, false},
		{"no times, numbered alike", list(10, "", "2", "2", "2"), list(11, "", "2", "2", "2"), 0, false},
		{"no times, and nothing shared", list(10, "", "2", "2"), list(40, "", "2", "2"), 0, false},
		{"no times, cut differently", list(10, "", "2", "2", "2"), list(10, "", "2", "3", "1"), 0, true},
		{"no times, and lengths a little apart", list(10, "", "2", "2.001", "2"), list(10, "", "2", "2", "2"), 0, false},
		{"nothing listed", list(10, "", "2"), list(10, ""), 0, true},
	} {
		if got, ok := test.p.ahead(test.other); ok == test.fails || got != test.want {
			t.Errorf("%s: ahead() = %d, %t; want %d, %t", test.name, got, ok, test.want, !test.fails)
		}
	}
}
