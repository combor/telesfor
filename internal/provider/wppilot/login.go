package wppilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
)

const (
	codeExpired = "The code expired before it was entered."

	// consents is what stands in the way of an account that has yet to
	// accept them: WP plays it nothing, whatever it lists.
	consents = "WP Pilot plays nothing until the account accepts its consents. Tick them at pilot.wp.pl/ustawienia/zgody-rodo/."

	// The cookies that WP's own site keeps a session in.
	sessionID  = "netviapisessid"
	sessionVal = "netviapisessval"
)

var errExpired = errors.New("the sign-in has expired: sign in again on telesfor's settings page")

// account is a sign-in and the channels it gives, as kept in the store.
type account struct {
	ID       string    `json:"id"`  // the session: the cookie netviapisessid
	Val      string    `json:"val"` // and netviapisessval, which goes with it
	Channels []channel `json:"channels"`
	// Unplayable are the channels found to be DRM-protected or without a
	// stream that telesfor reads, by WP's numbers. WP's list does not tell.
	Unplayable []int `json:"unplayable,omitempty"`
}

// channel is a channel as WP Pilot lists it.
type channel struct {
	ID   int    `json:"id"` // WP's number for it
	Name string `json:"name"`
	Logo string `json:"logo,omitempty"`
}

// pending is a code the user has yet to enter.
type pending struct {
	code    string
	url     string // where to enter it
	expires time.Time
	cancel  context.CancelFunc
}

// SignIn implements provider.Account. The code is one of those WP Pilot's
// apps for TV sets show.
func (p *Provider) SignIn(ctx context.Context) error {
	p.mu.Lock()
	p.actions++
	action := p.actions
	p.mu.Unlock()

	var device struct{ Code, URL string }
	answer, err := p.call(ctx, nil, http.MethodPost, p.site+"/api/v1/user_auth/activate_code?device_type=web", struct{}{}, &device)
	if err == nil && (answer.status != http.StatusOK || device.Code == "" || device.URL == "") {
		err = fmt.Errorf("%s, without a code", strings.TrimSpace(http.StatusText(answer.status)+" "+answer.refused))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actions != action {
		return nil // a sign-in or sign-out since then stands, not this one
	}
	if err != nil {
		p.problem = "WP gave no code to sign in with. telesfor's log has the reason."
		return fmt.Errorf("wppilot: asking for a sign-in code: %w", err)
	}
	if p.pending != nil {
		p.pending.cancel()
	}
	// WP names the page without saying how to reach it.
	if !strings.Contains(device.URL, "://") {
		device.URL = "https://" + device.URL
	}
	// The wait outlasts the request that started it.
	waiting, cancel := context.WithTimeout(context.Background(), p.codeLife)
	wait := &pending{code: device.Code, url: device.URL, expires: time.Now().Add(p.codeLife), cancel: cancel}
	p.pending, p.problem = wait, ""
	go p.await(waiting, wait)
	return nil
}

// await waits for the user to enter the code, then takes up their account.
func (p *Provider) await(ctx context.Context, wait *pending) {
	defer wait.cancel()
	signedIn, problem := p.authorize(ctx, wait.code)

	p.mu.Lock()
	if p.pending != wait { // given up, or a newer code took its place
		p.mu.Unlock()
		return
	}
	p.pending, p.problem = nil, problem
	if signedIn != nil {
		if p.account != nil { // a sign-in renewed
			signedIn.Unplayable = p.account.Unplayable
		}
		p.account, p.expired = signedIn, false
		if err := save(p.db, signedIn); err != nil {
			slog.Error("wppilot: saving the sign-in", "err", err)
			p.problem = "The sign-in could not be saved, so it will not outlast a restart of telesfor."
		}
	}
	changed := p.changed
	p.mu.Unlock()

	if signedIn != nil && changed != nil {
		changed()
	}
}

// authorize asks WP about a code until the user has entered it, or its time
// is up. It returns the account, and what stands in its way or went wrong, in
// words for the user.
func (p *Provider) authorize(ctx context.Context, code string) (*account, string) {
	for {
		var user struct {
			Consents bool `json:"needs_gdpr"` // has yet to accept them
		}
		answer, err := p.call(ctx, nil, http.MethodPost, p.site+"/api/v1/user_auth/verify_code?device_type=web", map[string]string{"code": code}, &user)
		switch {
		case err != nil:
			// The network's fault, or the code's time is up: the wait below tells.
		case answer.status == http.StatusOK && answer.id != "" && answer.val != "":
			// The user has done their part, so this is no longer bound by the code's time.
			signedIn := &account{ID: answer.id, Val: answer.val}
			channels, err := p.list(context.WithoutCancel(ctx), signedIn)
			if err != nil {
				slog.Error("wppilot: listing channels", "err", err)
				return nil, "WP accepted the code, but WP Pilot's channels could not be listed. Sign in again."
			}
			if len(channels) == 0 {
				return nil, "WP accepted the code, but WP Pilot lists no free channel for the account."
			}
			signedIn.Channels = channels
			if user.Consents {
				return signedIn, consents
			}
			return signedIn, ""
		case answer.refused == "code_need_verify":
			// The user has yet to enter the code.
		case answer.refused == "code_not_exists":
			return nil, codeExpired
		case answer.status == http.StatusTooManyRequests || answer.status >= http.StatusInternalServerError:
			// WP's trouble, not the code's.
		default:
			return nil, "WP refused the sign-in: " + strings.TrimSpace(http.StatusText(answer.status)+" "+answer.refused) + "."
		}
		select {
		case <-ctx.Done():
			return nil, codeExpired
		case <-time.After(p.poll):
		}
	}
}

// SignOut implements provider.Account. It gives up the code the user has yet
// to enter, if there is one, and forgets the account otherwise.
func (p *Provider) SignOut() error {
	p.mu.Lock()
	p.actions++
	if p.pending != nil {
		p.pending.cancel()
		p.pending = nil
		p.mu.Unlock()
		return nil
	}
	// Out of the store first: an account forgotten here alone would be back
	// after a restart.
	if err := save(p.db, nil); err != nil {
		p.problem = "The sign-in could not be removed from telesfor's data. telesfor's log has the reason."
		p.mu.Unlock()
		return fmt.Errorf("wppilot: forgetting the sign-in: %w", err)
	}
	signedIn := p.account != nil
	p.account, p.expired, p.problem = nil, false, ""
	changed := p.changed
	p.mu.Unlock()

	if signedIn && changed != nil {
		changed()
	}
	return nil
}

// Login implements provider.Account.
func (p *Provider) Login() provider.Login {
	p.mu.Lock()
	defer p.mu.Unlock()
	login := provider.Login{Problem: p.problem}
	switch {
	case p.pending != nil:
		login.State, login.Code, login.URL, login.Expires = provider.Pending, p.pending.code, p.pending.url, p.pending.expires
	case p.account == nil:
	case p.expired:
		login.State = provider.Expired
	default:
		login.State = provider.SignedIn
	}
	return login
}

// OnChange implements provider.Account.
func (p *Provider) OnChange(changed func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changed = changed
}

// verify asks WP whether the account has accepted the consents, and notes
// that WP accepts its session when it answers.
func (p *Provider) verify(ctx context.Context) {
	p.mu.Lock()
	signedIn := p.account
	p.mu.Unlock()
	if signedIn == nil {
		return
	}
	var user struct {
		Type     string
		Consents bool `json:"needs_gdpr"`
	}
	answer, err := p.call(ctx, signedIn, http.MethodGet, p.site+"/api/v2/user?device_type=web", nil, &user)
	// To a session it does not know, WP answers here as to a guest: with no
	// user. Whether it knows the session is for a call that it refuses to a
	// guest to tell, as relist's is: see call.
	if err != nil || answer.status != http.StatusOK || user.Type == "" {
		return
	}
	p.mu.Lock()
	if p.account == signedIn {
		p.expired = false
	}
	p.mu.Unlock()
	p.settle(signedIn, user.Consents)
}

// settle notes whether the consents stand in the account's way.
func (p *Provider) settle(signedIn *account, needed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.account != signedIn:
	case needed && p.problem == "":
		p.problem = consents
	case !needed && p.problem == consents:
		p.problem = ""
	}
}

// expire notes that WP no longer accepts the session of signedIn, unless
// another account has taken its place since.
func (p *Provider) expire(signedIn *account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account == signedIn {
		p.expired = true
	}
}

// renew takes up the session cookies an answer to signedIn has set.
func (p *Provider) renew(signedIn *account, id, val string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := *signedIn
	if id != "" {
		signedIn.ID = id
	}
	if val != "" {
		signedIn.Val = val
	}
	// Not an account that has signed out since: it would be back in the store.
	if (signedIn.ID != was.ID || signedIn.Val != was.Val) && p.account == signedIn {
		if err := save(p.db, signedIn); err != nil {
			slog.Error("wppilot: saving the session", "err", err)
		}
	}
}

// list asks WP for the channels the account gets free, by WP's numbers.
// Radio stations are left out, and so are the channels of elsewhere.
func (p *Provider) list(ctx context.Context, signedIn *account) ([]channel, error) {
	var all []struct {
		ID     int
		Name   string
		Access string `json:"access_status"`
		Radio  bool   `json:"is_audio_only"`
		Icon   struct{ Dark string }
	}
	answer, err := p.call(ctx, signedIn, http.MethodGet, p.site+"/api/v3/channels/list?device_type=web", nil, &all)
	if err == nil && answer.status != http.StatusOK {
		err = errors.New(strings.TrimSpace(http.StatusText(answer.status) + " " + answer.refused))
	}
	if err != nil {
		return nil, err
	}
	var channels []channel
	for _, ch := range all {
		if ch.ID > 0 && ch.Access == "free" && !ch.Radio && !slices.Contains(elsewhere, ch.ID) {
			channels = append(channels, channel{ID: ch.ID, Name: ch.Name, Logo: ch.Icon.Dark})
		}
	}
	slices.SortFunc(channels, func(a, b channel) int { return a.ID - b.ID })
	return channels, nil
}

// relist brings the account's channels up to date with WP's list. A list
// that cannot be had, or is empty, takes none away.
func (p *Provider) relist(ctx context.Context) {
	p.mu.Lock()
	signedIn := p.account
	p.mu.Unlock()
	if signedIn == nil {
		return
	}
	channels, err := p.list(ctx, signedIn)
	if err != nil || len(channels) == 0 {
		return
	}
	p.mu.Lock()
	if p.account != signedIn || slices.Equal(channels, signedIn.Channels) {
		p.mu.Unlock()
		return
	}
	signedIn.Channels = channels
	err = save(p.db, signedIn)
	changed := p.changed
	p.mu.Unlock()

	if err != nil {
		slog.Error("wppilot: saving the channels", "err", err)
	}
	if changed != nil {
		changed()
	}
}

var (
	bucket     = []byte("wppilot")
	accountKey = []byte("account")
)

// load reads the account from the store. There is none before the first
// sign-in.
func load(db *bolt.DB) (*account, error) {
	if db == nil {
		return nil, nil
	}
	var signedIn *account
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil || b.Get(accountKey) == nil {
			return nil
		}
		signedIn = new(account)
		return json.Unmarshal(b.Get(accountKey), signedIn)
	})
	if err != nil {
		return nil, fmt.Errorf("wppilot: loading the sign-in from %s: %w", db.Path(), err)
	}
	return signedIn, nil
}

// save writes the account to the store, or removes it when there is none.
func save(db *bolt.DB, signedIn *account) error {
	if db == nil {
		return nil
	}
	return db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		if signedIn == nil {
			return b.Delete(accountKey)
		}
		v, err := json.Marshal(signedIn)
		if err != nil {
			return err
		}
		return b.Put(accountKey, v)
	})
}

var _ provider.Account = (*Provider)(nil)
