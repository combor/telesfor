package provider

import "time"

// block is how long a placeholder lasts in the guide.
const block = time.Hour

// Fill returns the programmes of a channel with a placeholder wherever there
// is none between from and to: the channel's name, an hour at a time by the
// clock, described as unlisted. Plex offers a channel by what is on it, so a
// channel with nothing on has nothing to pick there.
//
// The known programmes are in the order they start.
func Fill(ch Channel, known []Programme, from, to time.Time, unlisted string) []Programme {
	var programmes []Programme
	at := from
	until := func(next time.Time) {
		for at.Before(next) {
			stop := at.Truncate(block).Add(block)
			if stop.After(next) {
				stop = next
			}
			programmes = append(programmes, Programme{
				ChannelID: ch.ID, Title: ch.Name, Description: unlisted, Start: at, Stop: stop,
			})
			at = stop
		}
	}
	for _, programme := range known {
		until(programme.Start)
		programmes = append(programmes, programme)
		if programme.Stop.After(at) {
			at = programme.Stop
		}
	}
	until(to)
	return programmes
}
