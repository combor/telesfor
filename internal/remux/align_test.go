package remux

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The PIDs of the stream the tests build.
const (
	tablePID = 0x1000 // the program map table
	videoPID = 0x100
	audioPID = 0x101
)

// packet builds an MPEG-TS packet. A keyframe is marked the way ffmpeg marks
// one, as a random access point. The payload tells packets apart.
func packet(pid int, begins, keyframe bool, payload ...byte) []byte {
	p := []byte{0x47, byte(pid >> 8), byte(pid), 0x10}
	if begins {
		p[1] |= 0x40
	}
	if keyframe {
		p[3] |= 0x20
		p = append(p, 1, 0x40) // an adaptation field of one byte: its flags
	}
	p = append(p, payload...)
	for len(p) < packetSize {
		p = append(p, 0xff)
	}
	return p
}

// programMapPacket builds the table that lists the streams, with the clock on
// the video.
func programMapPacket(streams ...int) []byte {
	section := []byte{
		0x02, 0, 0, // table id; the section's length, filled in below
		0, 1, 0xc1, 0, 0, // program number, version, section numbers
		0xe0 | videoPID>>8, videoPID & 0xff, // the stream that carries the clock
		0xf0, 0, // no descriptors of the program
	}
	for _, pid := range streams {
		section = append(section, 0x1b, 0xe0|byte(pid>>8), byte(pid), 0xf0, 0) // type, PID, no descriptors
	}
	section = append(section, 0, 0, 0, 0) // a checksum that nobody checks
	section[1], section[2] = 0xb0|byte((len(section)-3)>>8), byte(len(section)-3)
	return packet(tablePID, true, false, append([]byte{0}, section...)...)
}

// The packets the test streams are made of.
var (
	pat = packet(0, true, false,
		0,                // the section begins right here
		0x00, 0xb0, 0x0d, // table id; the section's length
		0, 1, 0xc1, 0, 0, // transport stream id, version, section numbers
		0, 1, 0xe0|tablePID>>8, tablePID&0xff, // program 1 is described at tablePID
		0, 0, 0, 0, // a checksum that nobody checks
	)
	pmt   = programMapPacket(videoPID, audioPID)
	key1  = packet(videoPID, true, true, 'k', 1) // a keyframe begins
	key2  = packet(videoPID, true, true, 'k', 2)
	frame = packet(videoPID, true, false, 'f')  // another frame begins
	more  = packet(videoPID, false, false, 'm') // more of a frame
	aud1  = packet(audioPID, true, false, 'a', 1)
	aud2  = packet(audioPID, true, false, 'a', 2)
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
			name: "streams that start together pass untouched",
			in:   [][]byte{pat, pmt, key1, more, aud1, frame, aud2},
			want: [][]byte{pat, pmt, key1, more, aud1, frame, aud2},
		},
		{
			name: "video first: the output starts at the last keyframe before the audio",
			in:   [][]byte{pat, pmt, key1, more, frame, key2, more, aud1, frame},
			want: [][]byte{pat, pmt, key2, more, aud1, frame},
		},
		{
			name: "audio first: the output starts at the first keyframe",
			in:   [][]byte{pat, pmt, aud1, aud2, key1, aud1, more},
			want: [][]byte{pat, pmt, key1, aud1, more},
		},
		{
			name: "video that begins between keyframes is kept from its start",
			in:   [][]byte{pat, pmt, frame, more, aud1, key1},
			want: [][]byte{pat, pmt, frame, more, aud1, key1},
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

	// The test stream has a keyframe every second, so the video may start that
	// much before the audio, and no more.
	if after := lag(out.Bytes()); after < -0.1 || after > 1.1 {
		t.Errorf("after aligning, the audio starts %.2f s after the video; want them to start together", after)
	}
	if out.Len()%packetSize != 0 || out.Bytes()[0] != 0x47 {
		t.Errorf("after aligning, the output of %d bytes is not whole MPEG-TS packets", out.Len())
	}
}
