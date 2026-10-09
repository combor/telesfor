// Package providertest has what the tests of providers share.
package providertest

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// Await waits for an account's sign-in to reach a state.
func Await(t *testing.T, p provider.Account, want provider.LoginState) provider.Login {
	t.Helper()
	for range 2000 {
		if login := p.Login(); login.State == want {
			return login
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("sign-in stands at %+v, want state %d", p.Login(), want)
	return provider.Login{}
}

// Covered checks that a channel's programmes in a guide run from from to to
// with no gap, each with a title and a start before its stop, and returns
// them. Each must start by the stop of the one before it.
func Covered(t *testing.T, guide []provider.Programme, channel provider.Channel, from, to time.Time) []provider.Programme {
	t.Helper()
	var programmes []provider.Programme
	covered := from // the guide has no gap up to here
	for _, programme := range guide {
		if programme.ChannelID != channel.ID {
			continue
		}
		if programme.Title == "" || !programme.Start.Before(programme.Stop) || programme.Start.After(covered) {
			t.Errorf("programme %+v, want a title, a start before its stop and no gap after %s", programme, covered)
		}
		programmes = append(programmes, programme)
		covered = programme.Stop
	}
	if covered.Before(to) {
		t.Errorf("%s: the guide ends at %s, want it to reach %s", channel.Name, covered, to)
	}
	return programmes
}

// Get fetches address with client, and returns the answer with its body.
func Get(t *testing.T, client *http.Client, address string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}
