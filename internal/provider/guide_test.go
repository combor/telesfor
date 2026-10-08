package provider

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// clock returns the time hours and minutes after midnight on Monday the 5th
// of October 2026, in UTC.
func clock(hours, minutes int) time.Time {
	return time.Date(2026, 10, 5, hours, minutes, 0, 0, time.UTC)
}

// shown writes programmes out a line each: whose, when, and what.
func shown(programmes []Programme) []string {
	var lines []string
	for _, programme := range programmes {
		line := fmt.Sprintf("%s %s to %s: %s", programme.ChannelID, programme.Start.Format("Mon 15:04"), programme.Stop.Format("Mon 15:04"), programme.Title)
		if programme.Description != "" {
			line += " | " + programme.Description
		}
		lines = append(lines, line)
	}
	return lines
}

func TestFill(t *testing.T) {
	ch := Channel{ID: "one", Name: "One"}
	known := []Programme{
		{ChannelID: "one", Title: "News", Start: clock(9, 0), Stop: clock(10, 15)}, // on since before from
		{ChannelID: "one", Title: "Film", Start: clock(11, 40), Stop: clock(13, 10)},
		{ChannelID: "one", Title: "Short", Start: clock(13, 0), Stop: clock(13, 30)}, // starts before the film ends
		{ChannelID: "one", Title: "Night", Start: clock(15, 0), Stop: clock(17, 0)},  // on until after to
	}
	tests := []struct {
		name  string
		known []Programme
		want  []string
	}{
		{"nothing known", nil, []string{
			"one Mon 09:30 to Mon 10:00: One | unlisted",
			"one Mon 10:00 to Mon 11:00: One | unlisted",
			"one Mon 11:00 to Mon 12:00: One | unlisted",
			"one Mon 12:00 to Mon 13:00: One | unlisted",
			"one Mon 13:00 to Mon 14:00: One | unlisted",
			"one Mon 14:00 to Mon 15:00: One | unlisted",
			"one Mon 15:00 to Mon 16:00: One | unlisted",
		}},
		{"before and after", known[1:2], []string{
			"one Mon 09:30 to Mon 10:00: One | unlisted",
			"one Mon 10:00 to Mon 11:00: One | unlisted",
			"one Mon 11:00 to Mon 11:40: One | unlisted",
			"one Mon 11:40 to Mon 13:10: Film",
			"one Mon 13:10 to Mon 14:00: One | unlisted",
			"one Mon 14:00 to Mon 15:00: One | unlisted",
			"one Mon 15:00 to Mon 16:00: One | unlisted",
		}},
		{"between", known, []string{
			"one Mon 09:00 to Mon 10:15: News",
			"one Mon 10:15 to Mon 11:00: One | unlisted",
			"one Mon 11:00 to Mon 11:40: One | unlisted",
			"one Mon 11:40 to Mon 13:10: Film",
			"one Mon 13:00 to Mon 13:30: Short",
			"one Mon 13:30 to Mon 14:00: One | unlisted",
			"one Mon 14:00 to Mon 15:00: One | unlisted",
			"one Mon 15:00 to Mon 17:00: Night",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := shown(Fill(ch, test.known, clock(9, 30), clock(16, 0), "unlisted"))
			if !slices.Equal(got, test.want) {
				t.Errorf("Fill():\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(test.want, "\n"))
			}
		})
	}
}

func TestUntilNext(t *testing.T) {
	listed := []Programme{
		{Title: "Early", Start: clock(6, 0)},
		{Title: "Morning", Start: clock(8, 0)},
		{Start: clock(10, 0)},                   // nothing listed
		{Title: "Trailer", Start: clock(12, 0)}, // over as it starts
		{Title: "Lunch", Description: "Soup", Image: "lunch.jpg", Start: clock(12, 0)},
		{Title: "Evening", Start: clock(13, 0)}, // the guide lacks the day after
		{Title: "Tuesday", Start: clock(24+13, 0)},
		{Title: "Last", Start: clock(24+14, 0)},
	}
	tests := []struct {
		name     string
		from, to time.Time
		want     []string
	}{
		{"two days", clock(9, 0), clock(24+15, 0), []string{
			"one Mon 08:00 to Mon 10:00: Morning",
			"one Mon 12:00 to Mon 13:00: Lunch | Soup",
			"one Tue 13:00 to Tue 14:00: Tuesday",
		}},
		{"a morning", clock(9, 0), clock(12, 0), []string{
			"one Mon 08:00 to Mon 10:00: Morning",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			programmes := UntilNext("one", listed, test.from, test.to)
			if got := shown(programmes); !slices.Equal(got, test.want) {
				t.Errorf("UntilNext():\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(test.want, "\n"))
			}
			for _, programme := range programmes {
				if programme.Title == "Lunch" && programme.Image != "lunch.jpg" {
					t.Errorf("Lunch has the picture %q, want the guide's", programme.Image)
				}
			}
		})
	}
}
