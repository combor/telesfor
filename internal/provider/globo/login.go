package globo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/store"
)

// activationURL is where the user enters a code. It is the page Globoplay's
// apps for TV sets send their users to.
const activationURL = "https://globoplay.globo.com/ativar"

const codeExpired = "The code expired before it was entered."

// account is a sign-in and the channels it gives, as kept in the store.
type account struct {
	GLBID    string    `json:"glbid"` // the session, which Globo's own sites keep in a cookie of this name
	Channels []channel `json:"channels"`
}

// pending is a code the user has yet to enter.
type pending struct {
	code    string
	expires time.Time
	cancel  context.CancelFunc
}

// SignIn implements provider.Account.
func (p *Provider) SignIn(ctx context.Context) error {
	p.mu.Lock()
	p.actions++
	action := p.actions
	p.mu.Unlock()

	var device struct {
		Code  string `json:"user_code"`
		Token string // what to ask about the code with
	}
	request := map[string]string{"product_name": "Globoplay", "device_name": "telesfor"}
	status, err := p.call(ctx, http.MethodPost, p.login+"/api/device/request-login/for/"+service, nil, request, &device)
	if err == nil && (status != http.StatusOK || device.Code == "" || device.Token == "") {
		err = fmt.Errorf("%s, without a code", http.StatusText(status))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actions != action {
		return nil // a sign-in or sign-out since then stands, not this one
	}
	if err != nil {
		p.problem = "Globo gave no code to sign in with. telesfor's log has the reason."
		return fmt.Errorf("globo: asking for a sign-in code: %w", err)
	}
	if p.pending != nil {
		p.pending.cancel()
	}
	// The wait outlasts the request that started it.
	waiting, cancel := context.WithTimeout(context.Background(), p.codeLife)
	wait := &pending{code: device.Code, expires: time.Now().Add(p.codeLife), cancel: cancel}
	p.pending, p.problem = wait, ""
	go p.await(waiting, wait, device.Token)
	return nil
}

// await waits for the user to enter the code, then takes up their account.
func (p *Provider) await(ctx context.Context, wait *pending, token string) {
	defer wait.cancel()
	signedIn, problem := p.authorize(ctx, token)

	p.mu.Lock()
	if p.pending != wait { // given up, or a newer code took its place
		p.mu.Unlock()
		return
	}
	p.pending, p.problem = nil, problem
	if signedIn != nil {
		if p.account != nil { // a sign-in renewed
			signedIn.Channels = merge(p.account.Channels, signedIn.Channels)
		}
		p.account, p.expired = signedIn, false
		if err := save(p.db, signedIn); err != nil {
			slog.Error("globo: saving the sign-in", "err", err)
			p.problem = "The sign-in could not be saved, so it will not outlast a restart of telesfor."
		}
	}
	changed := p.changed
	p.mu.Unlock()

	if signedIn != nil && changed != nil {
		changed()
	}
}

// authorize asks Globo about a code until the user has entered it, or its
// time is up. It returns the account, or what went wrong in words for the
// user.
func (p *Provider) authorize(ctx context.Context, token string) (*account, string) {
	for {
		var auth struct{ GLBID string }
		status, err := p.call(ctx, http.MethodPost, p.login+"/api/device/auth-with-token/for/"+service+"?with_cookie=1",
			nil, map[string]string{"token": token}, &auth)
		switch {
		case err != nil:
			// The network's fault, or the code's time is up: the wait below tells.
		case status == http.StatusOK && auth.GLBID != "":
			// The user has done their part, so this is no longer bound by the code's time.
			var listing struct{ Broadcasts json.RawMessage }
			if err := p.query(context.WithoutCancel(ctx), "{ broadcasts { slug name mediaId logo } }", nil, &listing); err != nil {
				slog.Error("globo: listing channels", "err", err)
				return nil, "Globo accepted the code, but Globoplay's channels could not be listed. Sign in again."
			}
			channels := list(listing.Broadcasts)
			if len(channels) == 0 {
				return nil, "Globo accepted the code, but Globoplay lists none of the channels telesfor offers."
			}
			return &account{GLBID: auth.GLBID, Channels: channels}, ""
		case status == http.StatusUnauthorized:
			// The user has yet to enter the code.
		case status == http.StatusNotFound:
			return nil, codeExpired
		default:
			return nil, "Globo refused the sign-in: " + http.StatusText(status) + "."
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
		return fmt.Errorf("globo: forgetting the sign-in: %w", err)
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
		login.State, login.Code, login.URL, login.Expires = provider.Pending, p.pending.code, activationURL, p.pending.expires
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

// verify asks Globo whether it still accepts the account's session.
func (p *Provider) verify(ctx context.Context) {
	p.mu.Lock()
	signedIn := p.account
	p.mu.Unlock()
	if signedIn == nil {
		return
	}
	status, err := p.call(ctx, http.MethodGet, p.login+"/api/user", map[string]string{"Cookie": "GLBID=" + signedIn.GLBID}, nil, nil)
	if err != nil {
		return // the network's fault says nothing about the session
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account != signedIn {
		return
	}
	switch status {
	case http.StatusOK:
		p.expired = false
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound: // measured: 404
		p.expired = true
	}
}

// expire notes that Globo no longer accepts the session of signedIn, unless
// another account has taken its place since.
func (p *Provider) expire(signedIn *account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account == signedIn {
		p.expired = true
	}
}

// load reads the account from the store. There is none before the first
// sign-in.
func load(db *bolt.DB) (*account, error) {
	signedIn := new(account)
	found, err := store.Get(db, "globo", "account", signedIn)
	if err != nil {
		return nil, fmt.Errorf("globo: loading the sign-in: %w", err)
	}
	if !found {
		return nil, nil
	}
	return signedIn, nil
}

// save writes the account to the store, or removes it when there is none.
func save(db *bolt.DB, signedIn *account) error {
	if signedIn == nil {
		return store.Delete(db, "globo", "account")
	}
	return store.Put(db, "globo", "account", signedIn)
}

var _ provider.Account = (*Provider)(nil)
