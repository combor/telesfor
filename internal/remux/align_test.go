package remux

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// names renders a stream packet by packet, to make a failed test readable.
func names(stream []byte) string {
	var names []string
	for ; len(stream) >= packetSize; stream = stream[packetSize:] {
		p := stream[:packetSize]
		switch int(p[1]&0x1f)<<8 | int(p[2]) {
		case 0:
			names = append(names, "pat")
		case tablePID:
			names = append(names, "pmt")
		default:
			payload := p[4:]
			if p[3]&0x20 != 0 {
				payload = p[6:]
			}
			name := string(payload[0])
			if payload[1] != 0xff {
				name += strconv.Itoa(int(payload[1]))
			}
			names = append(names, name)
		}
	}
	return strings.Join(names, " ")
}

func TestAligner(t *testing.T) {
	tests := []struct {
		name     string
		in, want [][]byte
	}{
		{
			name: "streams that start together: the output starts at the next keyframe, audio first",
			in:   [][]byte{pat, pmt, key1, more, aud1, frame, aud2, more, key2, more, aud1},
			want: [][]byte{pat, pmt, aud2, key2, more, aud1},
		},
		{
			name: "video first: the output starts at the first keyframe after the audio",
			in:   [][]byte{pat, pmt, key1, more, frame, key2, more, aud1, frame, key1, more},
			want: [][]byte{pat, pmt, aud1, key1, more},
		},
		{
			name: "audio first: the output starts at the first keyframe",
			in:   [][]byte{pat, pmt, aud1, aud2, key1, aud1, more},
			want: [][]byte{pat, pmt, aud2, key1, aud1, more},
		},
		{
			name: "the audio in flight is kept whole, and audio cut off at its start is dropped",
			in:   [][]byte{pat, pmt, rest, key1, aud1, rest, frame, aud2, rest, more, key2, rest, more},
			want: [][]byte{pat, pmt, aud2, rest, key2, rest, more},
		},
		{
			name: "video before its first keyframe is dropped",
			in:   [][]byte{pat, pmt, frame, more, aud1, key1, more},
			want: [][]byte{pat, pmt, aud1, key1, more},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in, want := bytes.Join(test.in, nil), bytes.Join(test.want, nil)

			var whole bytes.Buffer
			newAligner(&whole).Write(in)
			if !bytes.Equal(whole.Bytes(), want) {
				t.Errorf("got  %s\nwant %s", names(whole.Bytes()), names(want))
			}

			// ffmpeg's writes do not end where packets end.
			var pieces bytes.Buffer
			a := newAligner(&pieces)
			for ; len(in) > 0; in = in[min(100, len(in)):] {
				a.Write(in[:min(100, len(in))])
			}
			if !bytes.Equal(pieces.Bytes(), want) {
				t.Errorf("written in pieces: got %s\nwant %s", names(pieces.Bytes()), names(want))
			}
		})
	}
}

// When the aligner cannot do its job, the stream must still get through.
func TestAlignerGivesUp(t *testing.T) {
	t.Run("a stream that never starts", func(t *testing.T) {
		in := bytes.Join([][]byte{pat, pmt, key1, more, more, more, more, frame, more}, nil)

		var out bytes.Buffer
		a := newAligner(&out)
		a.limit = 5 * packetSize // far less than the audio would need to show up
		a.Write(in)
		if !bytes.Equal(out.Bytes(), in) {
			t.Errorf("got  %s\nwant %s", names(out.Bytes()), names(in))
		}
	})
	t.Run("not MPEG-TS", func(t *testing.T) {
		in := bytes.Repeat([]byte("not a transport stream. "), 20)

		var out bytes.Buffer
		newAligner(&out).Write(in)
		if !bytes.Equal(out.Bytes(), in) {
			t.Errorf("got %d bytes, want the %d that went in, unchanged", out.Len(), len(in))
		}
	})
}

// TestAlignerOnFFmpegOutput checks the aligner against the real thing: a stream
// from ffmpeg whose audio starts three seconds after its video.
func TestAlignerOnFFmpegOutput(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	stream, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=160x120:rate=25",
		"-itsoffset", "3", "-f", "lavfi", "-i", "sine=duration=3",
		"-c:v", "mpeg2video", "-g", "25", "-c:a", "mp2",
		"-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg cannot generate a test stream: %v", err)
	}

	// lag is how long after the video the audio of a stream starts, in seconds.
	lag := func(stream []byte) float64 {
		t.Helper()
		probe := exec.Command(ffprobe, "-hide_banner", "-loglevel", "error", "-f", "mpegts", "-i", "pipe:0",
			"-show_entries", "stream=codec_type,start_time", "-of", "csv=p=0")
		probe.Stdin = bytes.NewReader(stream)
		out, err := probe.Output()
		if err != nil {
			t.Fatal(err)
		}
		start := map[string]float64{}
		for _, line := range strings.Fields(string(out)) { // lines like "video,1.440000,"
			fields := strings.Split(line, ",")
			at, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				t.Fatalf("ffprobe said %q: %v", line, err)
			}
			start[fields[0]] = at
		}
		if len(start) != 2 {
			t.Fatalf("ffprobe found %q, want a video and an audio stream", out)
		}
		return start["audio"] - start["video"]
	}
	if before := lag(stream); before < 2.5 {
		t.Fatalf("the test stream's audio starts %.2f s after its video, want about 3", before)
	}

	var out bytes.Buffer
	newAligner(&out).Write(stream)

	// The audio must lead, by no more than what was in flight at the keyframe.
	if after := lag(out.Bytes()); after > 0 || after < -0.5 {
		t.Errorf("after aligning, the audio starts %.2f s after the video; want it to start just before", after)
	}
	if out.Len()%packetSize != 0 || out.Bytes()[0] != 0x47 {
		t.Errorf("after aligning, the output of %d bytes is not whole MPEG-TS packets", out.Len())
	}
}
