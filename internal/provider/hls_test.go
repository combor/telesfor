package provider

import (
	"strings"
	"testing"
)

func TestBestQuality(t *testing.T) {
	master := `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-MEDIA:TYPE=AUDIO,URI="audio.m3u8",GROUP-ID="audio",NAME="Undetermined",DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=1191608,AVERAGE-BANDWIDTH=1083279,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=576x324,AUDIO="audio"
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2824007,AVERAGE-BANDWIDTH=2567279,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,AUDIO="audio"
high.m3u8?session=1
#EXT-X-STREAM-INF:BANDWIDTH=1824000,RESOLUTION=960x540
middle.m3u8
`
	tests := []struct {
		name, playlist, want string
	}{
		{"master", master, "high.m3u8?session=1"},
		{"lines ended in CRLF", strings.ReplaceAll(master, "\n", "\r\n"), "high.m3u8?session=1"},
		{"a blank line and a comment before the URI", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000\n\n# the only one\nonly.m3u8\n", "only.m3u8"},
		{"no bandwidths", "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\nfirst.m3u8\n#EXT-X-STREAM-INF:RESOLUTION=1920x1080\nsecond.m3u8\n", "first.m3u8"},
		{"a quality's playlist", "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXTINF:4.004,\nsegment.ts\n", ""},
		{"empty", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := BestQuality(test.playlist); got != test.want {
				t.Errorf("BestQuality() = %q, want %q", got, test.want)
			}
		})
	}
}
