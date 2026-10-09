package remux

import (
	"net/url"
	"strings"
	"testing"
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
