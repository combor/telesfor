package provider

import (
	"strconv"
	"strings"
)

// BestQuality returns the URI of the highest quality in a master playlist, by
// its BANDWIDTH, or "" for a playlist that is not a master.
func BestQuality(master string) (uri string) {
	// The bandwidth of the best quality so far, and of the one a tag has
	// announced: its URI is the next line that is not a comment.
	most, announced := -1, -1
	for line := range strings.Lines(master) {
		line = strings.TrimSpace(line)
		if attributes, ok := strings.CutPrefix(line, "#EXT-X-STREAM-INF:"); ok {
			announced = 0
			for attribute := range strings.SplitSeq(attributes, ",") {
				if bandwidth, ok := strings.CutPrefix(attribute, "BANDWIDTH="); ok {
					announced, _ = strconv.Atoi(bandwidth)
				}
			}
		} else if announced >= 0 && line != "" && !strings.HasPrefix(line, "#") {
			if announced > most {
				most, uri = announced, line
			}
			announced = -1
		}
	}
	return uri
}
