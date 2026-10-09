package provider

import (
	"strconv"
	"strings"
)

// BestQuality returns the URI of the highest quality in a master playlist, or
// "" for a playlist that is not a master.
func BestQuality(master string) (uri string) {
	// announced is the bandwidth of a tag whose URI is yet to come, or -1.
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
