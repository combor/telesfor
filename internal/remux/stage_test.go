package remux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

// What the stream of a station is made of.
const (
	stationSegments = 16                     // how many segments it is long
	stationSegment  = 500 * time.Millisecond // how long each plays
	stationFrames   = 10                     // how many frames are in each
	stationFrame    = 4500                   // how long each of those lasts, on the clock of MPEG-TS
)

// station is a provider's server for a test: a live stream that comes in two
// qualities, high and low, with its sound apart. Like the real ones, it
// starts with a few segments listed and lists another every time one's length
// has passed, here until the stream ends.
type station struct {
	dir      string                  // where ffmpeg wrote the stream
	lists    map[string][]string     // each playlist in pieces: what leads it, and then every segment's lines
	segments map[string]stationMedia // metadata by request path

	// trouble, if set, may answer for a segment in the station's place.
	trouble func(w http.ResponseWriter, r *http.Request, quality string, seq int, body []byte) bool
	missing string // a quality whose playlist is not there, if any

	start sync.Once
	began time.Time // when the stream was first asked for
}

type stationMedia struct {
	quality string
	seq     int
}

// newStation has ffmpeg make a stream with segments of the given kind, mpegts
// or fmp4.
func newStation(t *testing.T, ffmpeg, kind string) *station {
	t.Helper()
	dir := t.TempDir()
	length := strconv.FormatFloat((stationSegments * stationSegment).Seconds(), 'f', -1, 64)
	args := []string{"-hide_banner", "-loglevel", "error",
		// Noise, so that a segment is large enough to tell a speed by.
		"-f", "lavfi", "-i", "testsrc=duration=" + length + ":size=320x240:rate=20,noise=alls=40:allf=t",
		"-f", "lavfi", "-i", "sine=duration=" + length,
	}
	tracks := []struct {
		name string
		args []string
	}{
		{"high", []string{"-map", "0:v", "-c:v", "mpeg2video", "-g", "10", "-b:v", "2M"}},
		{"low", []string{"-map", "0:v", "-vf", "scale=160:120", "-c:v", "mpeg2video", "-g", "10", "-b:v", "800k"}},
		{"sound", []string{"-map", "1:a", "-c:a", "mp2"}},
	}
	for i, track := range tracks {
		// Neutral file names make the test independent of quality names.
		file := "media" + strconv.Itoa(i)
		args = append(args, track.args...)
		args = append(args, "-f", "hls", "-hls_time", "0.5", "-hls_playlist_type", "vod")
		if kind == "fmp4" {
			args = append(args, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", file+"init.mp4",
				"-hls_segment_filename", filepath.Join(dir, file+"-%d.m4s"))
		} else {
			args = append(args, "-hls_segment_filename", filepath.Join(dir, file+"-%d.ts"))
		}
		args = append(args, filepath.Join(dir, track.name+".m3u8"))
	}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot generate a test stream: %v\n%s", err, out)
	}

	s := &station{dir: dir, lists: map[string][]string{}, segments: map[string]stationMedia{}}
	for _, track := range tracks {
		name := track.name
		list, err := os.ReadFile(filepath.Join(dir, name+".m3u8"))
		if err != nil {
			t.Fatal(err)
		}
		lead, segments, _ := strings.Cut(strings.TrimSuffix(strings.TrimSpace(string(list)), "#EXT-X-ENDLIST"), "#EXTINF")
		lead = strings.ReplaceAll(lead, "#EXT-X-PLAYLIST-TYPE:VOD\n", "")
		s.lists[name] = []string{lead}
		for segment := range strings.SplitSeq(segments, "#EXTINF") {
			entry := "#EXTINF" + segment
			seq := len(s.lists[name]) - 1
			s.lists[name] = append(s.lists[name], entry)
			for line := range strings.Lines(entry) {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					s.segments["/"+line] = stationMedia{quality: name, seq: seq}
				}
			}
		}
		if name != "sound" && len(s.lists[name]) != 1+stationSegments {
			t.Fatalf("ffmpeg cut the %s quality into %d segments, want %d", name, len(s.lists[name])-1, stationSegments)
		}
	}
	return s
}

func (s *station) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.start.Do(func() { s.began = time.Now() })
	name := strings.TrimPrefix(r.URL.Path, "/")
	list, isList := strings.CutSuffix(name, ".m3u8")
	switch {
	case name == "master.m3u8":
		io.WriteString(w, "#EXTM3U\n"+
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"sound\",NAME=\"sound\",DEFAULT=YES,URI=\"sound.m3u8\"\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=160x120,AUDIO=\"sound\"\nlow.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=320x240,AUDIO=\"sound\"\nhigh.m3u8\n")
	case isList && list == s.missing:
		http.NotFound(w, r)
	case isList && s.lists[list] != nil:
		lines := s.lists[list]
		listed := headStart + int(time.Since(s.began)/stationSegment)
		if listed >= stationSegments { // the sound is cut elsewhere, and may be a segment longer
			io.WriteString(w, strings.Join(lines, "")+"#EXT-X-ENDLIST\n")
			return
		}
		io.WriteString(w, strings.Join(lines[:1+listed], ""))
	default:
		body, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		segment, known := s.segments[r.URL.Path]
		if known && s.trouble != nil && s.trouble(w, r, segment.quality, segment.seq, body) {
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	}
}

// watched is what a stream turned out to be, as ffprobe sees it.
type watched struct {
	widths []int // of its frames, counted: {320, 80, 160, 60} is 80 frames that wide and then 60 narrower
	frames int   // how many it has
}

// watch checks that a stream is one stream to a player, however many ffmpegs
// wrote it, and returns what is in it.
func watch(t *testing.T, ffprobe string, stream []byte) watched {
	t.Helper()
	probe := func(args ...string) []string {
		t.Helper()
		cmd := exec.Command(ffprobe, append([]string{"-hide_banner", "-loglevel", "error", "-f", "mpegts", "-i", "pipe:0", "-of", "csv=p=0"}, args...)...)
		cmd.Stdin = bytes.NewReader(stream)
		var complaints bytes.Buffer
		cmd.Stderr = &complaints
		out, err := cmd.Output()
		if err != nil || complaints.Len() > 0 {
			t.Errorf("ffprobe %s: %v\n%s", strings.Join(args, " "), err, complaints.Bytes())
		}
		return strings.Fields(string(out))
	}

	// Every frame is decoded a frame after the one before, and the sound goes
	// on evenly: its frames are 1152 samples of 44100 a second.
	decoded := map[string][]int{}
	for _, line := range probe("-show_entries", "packet=codec_type,dts") { // lines like "video,126000"
		kind, dts, _ := strings.Cut(strings.TrimSuffix(line, ","), ",")
		at, err := strconv.Atoi(dts)
		if err != nil {
			t.Fatalf("ffprobe said %q: %v", line, err)
		}
		decoded[kind] = append(decoded[kind], at)
	}
	for kind, step := range map[string]int{"video": stationFrame, "audio": 2351} {
		if len(decoded[kind]) == 0 {
			t.Fatalf("the stream has no %s", kind)
		}
		for i, at := range decoded[kind][1:] {
			if got := at - decoded[kind][i]; got < step-4 || got > step+4 { // each ffmpeg rounds for itself
				t.Errorf("%s frame %d comes %d after the one before, want %d", kind, i+1, got, step)
			}
		}
	}
	// The sound is as long as the picture, give or take what the start cut
	// off and a segment's worth at the end.
	video, audio := decoded["video"], decoded["audio"]
	if apart := (audio[len(audio)-1] - audio[0]) - (video[len(video)-1] - video[0]); apart < -90000 || apart > 90000 {
		t.Errorf("the sound is %d ticks longer than the picture, want them the same", apart)
	}

	var w watched
	for _, line := range probe("-select_streams", "v", "-show_entries", "frame=width") {
		width, err := strconv.Atoi(strings.TrimSuffix(line, ","))
		if err != nil {
			t.Fatalf("ffprobe said %q: %v", line, err)
		}
		if n := len(w.widths); n == 0 || w.widths[n-2] != width {
			w.widths = append(w.widths, width, 0)
		}
		w.widths[len(w.widths)-1]++
	}
	// ffprobe's decoder drops the frame it has in hand when the picture
	// changes size: one frame for every change, and from the quality before.
	decodable := 0
	for i := 1; i < len(w.widths); i += 2 {
		if i+1 < len(w.widths) {
			w.widths[i]++
		}
		decodable += w.widths[i]
	}
	if w.frames = len(video); decodable != w.frames {
		t.Errorf("%d frames can be decoded of %d, counting one lost to each change of size: %v", decodable, w.frames, w.widths)
	}

	checkContinuity(t, stream)
	return w
}

// TestCopySwitches plays a generated live stream that comes in two qualities
// end to end, with a provider that makes the quality change on the way.
func TestCopySwitches(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	const high, low = 320, 160 // how wide the qualities are

	for _, test := range []struct {
		name    string
		known   float64 // how fast the connection was measured to be before, if it was
		missing string  // a quality that the provider lists and does not have
		trouble func(w http.ResponseWriter, r *http.Request, quality string, seq int, body []byte) bool
		want    []int // the widths the stream goes through
		exactly int   // how many frames of the last are to come, if that is certain
		late    int   // how many segments later than usual the stream may start
		fmp4    bool  // whether to play it in fMP4 as well, which has more for a leg to carry over
	}{
		{
			// Nothing has been sent by then: the stream starts over.
			name: "a connection that is slow from the start",
			trouble: func(w http.ResponseWriter, r *http.Request, quality string, seq int, body []byte) bool {
				if quality == "sound" {
					return false
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				for piece := range slices.Chunk(body, 12500) { // a megabit a second: enough for the low quality
					w.Write(piece)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return true
					case <-time.After(100 * time.Millisecond):
					}
				}
				return true
			},
			want: []int{low},
			late: 4,
		},
		{
			name: "segments that come slowly",
			trouble: func(w http.ResponseWriter, r *http.Request, quality string, seq int, body []byte) bool {
				if quality == "high" && seq >= 8 {
					time.Sleep(1200 * time.Millisecond)
				}
				return false
			},
			want: []int{high, low},
			fmp4: true,
		},
		{
			// The same segment is then played in the lower quality.
			name: "a segment that stops coming",
			trouble: func(w http.ResponseWriter, r *http.Request, quality string, seq int, body []byte) bool {
				if quality != "high" || seq < 8 {
					return false
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Write(body[:len(body)/2])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return true
			},
			want:    []int{high, low},
			exactly: (stationSegments - 8) * stationFrames,
		},
		{
			name:  "a connection that turns out faster",
			known: 600e3,
			want:  []int{low, high},
		},
		{
			// The stream goes on in the quality it had, from where it was.
			name:    "a quality that will not play",
			missing: "low",
			trouble: func(w http.ResponseWriter, r *http.Request, quality string, seq int, body []byte) bool {
				if quality == "high" && seq >= 8 && seq < 12 {
					time.Sleep(1200 * time.Millisecond)
				}
				return false
			},
			want: []int{high},
		},
		{
			// The stream starts in the best then, as it does when nothing
			// is known of the connection.
			name:    "a quality that will not play from the start",
			known:   600e3,
			missing: "low",
			want:    []int{high},
		},
	} {
		kinds := []string{"mpegts"}
		if test.fmp4 {
			kinds = append(kinds, "fmp4")
		}
		for _, kind := range kinds {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				s := newStation(t, ffmpeg, kind)
				s.trouble, s.missing = test.trouble, test.missing
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					want := 2
					if _, segment := s.segments[req.URL.Path]; segment {
						want = 1
					}
					if req.ProtoMajor != want {
						t.Errorf("%s used %s, want HTTP/%d", req.URL.Path, req.Proto, want)
					}
					s.ServeHTTP(w, req)
				}))
				upstream.EnableHTTP2 = true
				upstream.StartTLS()
				defer upstream.Close()
				client := *upstream.Client()
				client.Transport = httpclient.NewTransport(client.Transport.(*http.Transport))
				defer client.CloseIdleConnections()

				remuxer, err := New()
				if err != nil {
					t.Fatal(err)
				}
				// Segments of half a second are no match for what Plex holds
				// back, or for half a minute of calm.
				remuxer.hold, remuxer.calm = 0, 0
				if test.known > 0 {
					remuxer.route("test").learn(test.known, time.Now())
				}
				var out bytes.Buffer
				stream := Stream{Manifest: upstream.URL + "/master.m3u8", Client: &client, Name: test.name, Route: "test"}
				if err := remuxer.Copy(t.Context(), &out, stream); err != nil {
					t.Fatal(err)
				}

				got := watch(t, ffprobe, out.Bytes())
				var widths []int
				for i := 0; i < len(got.widths); i += 2 {
					widths = append(widths, got.widths[i])
				}
				if !slices.Equal(widths, test.want) {
					t.Fatalf("the picture is %v wide (width, frames, …), want it to go through %v", got.widths, test.want)
				}
				// All of the stream is there but its start, which is cut to
				// where picture and sound begin together.
				if missing := stationSegments*stationFrames - got.frames; missing < 0 || missing > (2+test.late)*stationFrames || missing%stationFrames != 0 {
					t.Errorf("the stream has %d frames %v, want all %d but a segment or two of the start", got.frames, got.widths, stationSegments*stationFrames)
				}
				if last := got.widths[len(got.widths)-1]; test.exactly > 0 && last != test.exactly {
					t.Errorf("%d frames came in the last quality, want %d", last, test.exactly)
				}
			})
		}
	}
}

// gone is a viewer who takes no more of a stream.
type gone struct{}

func (gone) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A stream ends with a viewer who takes no more of it, whatever its ffmpeg
// has left to write: nobody is there to read that.
func TestCopyEndsWithItsViewer(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	upstream := httptest.NewServer(newStation(t, ffmpeg, "mpegts"))
	defer upstream.Close()
	remuxer, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ended := make(chan error, 1)
	go func() {
		ended <- remuxer.Copy(t.Context(), gone{}, Stream{Manifest: upstream.URL + "/master.m3u8", Client: upstream.Client()})
	}()
	select {
	case err := <-ended:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("Copy() = %v, want the error of the write", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Copy() goes on 10s after nothing more could be written")
	}
}

// Read audio through the stage rather than seeding the flow counter. This
// must fail if audio is disconnected from the shared speed measurement.
func TestAudioContributesToFlow(t *testing.T) {
	const chunk = 32 << 10
	resume := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "65536")
		w.Write(make([]byte, chunk))
		w.(http.Flusher).Flush()
		select {
		case <-resume:
		case <-req.Context().Done():
			return
		}
		w.Write(make([]byte, chunk))
	}))
	defer origin.Close()
	relay, _, err := openRelay(origin.URL+"/playlist", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	defer relay.segments.CloseIdleConnections()
	now := time.Now()
	ctl := testController(&route{}, &now)
	st, err := openStage(relay, tvpLadder, ctl, newMeter(io.Discard), func(*leg, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	l, err := st.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	address, _ := url.Parse(origin.URL + "/audio")
	l.sound.list = playlist{segments: []entry{{seq: 0, uri: address, length: time.Second}}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+st.server.Addr+"/1/a/0/asset", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.CopyN(io.Discard, resp.Body, chunk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * atOnce) // put the remaining bytes beyond the initial read burst
	close(resume)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	st.flow.mu.Lock()
	defer st.flow.mu.Unlock()
	var counted int64
	for _, n := range st.flow.tenths {
		counted += n
	}
	if counted < chunk {
		t.Fatalf("counted %d audio bytes, want at least %d", counted, chunk)
	}
}

// A segment that is not watched, of the sound or of the lowest quality, comes
// over HTTP/1.1 too, and stops coming once the viewer leaves or the leg is
// cut before it. Other requests still go over HTTP/2.
func TestUnwatchedSegmentCancellation(t *testing.T) {
	for _, test := range []struct {
		name, track string
		cut         bool
	}{
		{"audio, the viewer leaves", "a", false},
		{"audio, the leg is cut before the headers come", "a", true},
		{"video of the lowest quality, the viewer leaves", "v", false},
		{"video of the lowest quality, the leg is cut before the headers come", "v", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			asked, release, canceled := make(chan int, 1), make(chan struct{}), make(chan struct{})
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/next" {
					io.WriteString(w, "ok")
					return
				}
				asked <- req.ProtoMajor
				select {
				case <-release:
				case <-req.Context().Done():
					return
				}
				w.Header().Set("Content-Length", "1000000")
				w.Write(make([]byte, 32<<10))
				w.(http.Flusher).Flush()
				<-req.Context().Done()
				close(canceled)
			}))
			origin.EnableHTTP2 = true
			origin.StartTLS()
			defer origin.Close()
			client := &http.Client{Transport: httpclient.NewTransport(origin.Client().Transport.(*http.Transport))}
			defer client.CloseIdleConnections()
			relay, _, err := openRelay(origin.URL+"/playlist", client)
			if err != nil {
				t.Fatal(err)
			}
			defer relay.close()
			now := time.Now()
			ctl := testController(&route{}, &now)
			st, err := openStage(relay, tvpLadder, ctl, newMeter(io.Discard), func(*leg, string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer st.close()
			l, err := st.begin(0)
			if err != nil {
				t.Fatal(err)
			}
			address, _ := url.Parse(origin.URL + "/asset")
			list := playlist{segments: []entry{{seq: 0, uri: address, length: time.Second}}}
			l.video.list, l.sound.list = list, list

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+st.server.Addr+"/1/"+test.track+"/0/asset", nil)
			var resp *http.Response
			var fetchErr error
			answered := make(chan struct{})
			go func() {
				resp, fetchErr = http.DefaultClient.Do(req)
				close(answered)
			}()
			select {
			case protocol := <-asked:
				if protocol != 1 {
					t.Fatalf("unwatched segment used HTTP/%d, want HTTP/1", protocol)
				}
			case <-ctx.Done():
				t.Fatal("origin was not reached")
			}
			if test.cut {
				st.mu.Lock()
				l.cut()
				st.mu.Unlock()
			}
			close(release)
			<-answered // the request's deadline also bounds this wait
			if fetchErr != nil {
				t.Fatal(fetchErr)
			}
			defer resp.Body.Close()
			if test.cut {
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("cut segment returned %d", resp.StatusCode)
				}
			} else {
				if _, err := io.CopyN(io.Discard, resp.Body, 1); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			resp.Body.Close()
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream body was not canceled")
			}
			resp, err = client.Get(origin.URL + "/next")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.ProtoMajor != 2 {
				t.Errorf("ordinary request used %s", resp.Proto)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
		})
	}
}
