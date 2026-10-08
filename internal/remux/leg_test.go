package remux

import (
	"slices"
	"testing"
)

// The picture and the sound of a stream's first leg begin at the same time,
// though their playlists are read one after the other and a segment may be
// listed in between: ffmpeg does without a picture that begins long after the
// sound.
func TestFirstLegBeginsTogether(t *testing.T) {
	eight := slices.Repeat([]string{"2"}, 8)
	for _, test := range []struct {
		name         string
		video, sound playlist
		soundFirst   bool
		want         [2]int64 // where the picture and the sound begin
	}{
		{"the sound has a segment more", listOf(t, 100, "21:00:00Z", eight...), listOf(t, 100, "21:00:00Z", append(eight, "2")...), false, [2]int64{102, 102}},
		{"read the other way round", listOf(t, 100, "21:00:00Z", eight...), listOf(t, 100, "21:00:00Z", append(eight, "2")...), true, [2]int64{103, 103}},
		{"numbered apart", listOf(t, 100, "21:00:00Z", eight...), listOf(t, 500, "21:00:00Z", append(eight, "2")...), false, [2]int64{102, 502}},
		{"without times: as far from the newest", listOf(t, 100, "", eight...), listOf(t, 500, "", append(eight, "2")...), false, [2]int64{102, 503}},
	} {
		s := &stage{relay: &relay{}}
		l := &leg{}
		l.video.from, l.sound.from = -1, -1
		for _, video := range []bool{!test.soundFirst, test.soundFirst} {
			track, list := &l.sound, test.sound
			if video {
				track, list = &l.video, test.video
			}
			if !s.place(l, track, list, video) {
				t.Fatalf("%s: the leg could not be placed", test.name)
			}
			track.list = list
		}
		if got := [2]int64{l.video.from, l.sound.from}; got != test.want {
			t.Errorf("%s: picture and sound begin at %v, want %v", test.name, got, test.want)
		}
	}
}

// The picture and the sound of a leg end at the same time, though ffmpeg has
// asked for a segment less of the sound: the leg after it begins with both
// there.
func TestLegEndsTogether(t *testing.T) {
	eight := slices.Repeat([]string{"2"}, 8)
	for _, test := range []struct {
		name         string
		video, sound playlist
	}{
		{"by the clock", listOf(t, 100, "21:00:00Z", eight...), listOf(t, 500, "21:00:00Z", eight...)},
		{"without times: as many segments of each", listOf(t, 100, "", eight...), listOf(t, 500, "", eight...)},
	} {
		l := &leg{}
		l.video = track{list: test.video, from: 102, started: 104}
		l.sound = track{list: test.sound, from: 502, started: 503}
		l.cut()
		if got, want := [2]int64{l.video.until, l.sound.until}, [2]int64{105, 505}; got != want {
			t.Errorf("%s: picture and sound end before %v, want %v", test.name, got, want)
		}
	}
}
