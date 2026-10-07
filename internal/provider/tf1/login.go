package tf1

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
)

// TF1 signs a device in the way of RFC 8628, as its apps for TV sets do: the
// device asks for a code, the user enters it on tf1.fr, signed in there, and
// the device, which has been asking meanwhile, is handed tokens.
const (
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

	// margin is how long before its time is up a token is renewed.
	margin = 5 * time.Minute

	codeExpired = "The code expired before it was entered."
	unsaved     = "The sign-in could not be saved, so it will not outlast a restart of telesfor."
)

var errExpired = errors.New("the sign-in has expired: sign in again on telesfor's settings page")

// account is a sign-in, as kept in the store.
type account struct {
	Access  string    `json:"access"`  // the token the player's API takes
	Refresh string    `json:"refresh"` // gets the next two, once
	Expires time.Time `json:"expires"` // when the access token runs out; zero if TF1 did not say
}

// grant is TF1's answer to a request for tokens: the tokens, or why not.
type grant struct {
	Access  string  `json:"access_token"`
	Refresh string  `json:"refresh_token"`
	Life    float64 `json:"expires_in"` // of the access token, in seconds
	Error   string
}

// account is the sign-in that the grant makes, or nil if it grants none.
func (g grant) account() *account {
	if g.Access == "" || g.Refresh == "" {
		return nil
	}
	signedIn := &account{Access: g.Access, Refresh: g.Refresh}
	if g.Life > 0 {
		signedIn.Expires = time.Now().Add(seconds(g.Life))
	}
	return signedIn
}

func seconds(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }

// pending is a code the user has yet to enter.
type pending struct {
	code    string
	url     string // where to enter it
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
		Token    string  `json:"device_code"` // what to ask about the code with
		Code     string  `json:"user_code"`
		URL      string  `json:"verification_uri"`
		Complete string  `json:"verification_uri_complete"` // the same, with the code filled in
		Life     float64 `json:"expires_in"`                // of the code, in seconds
		Interval float64 // between two looks at it, in seconds
	}
	status, err := p.call(ctx, http.MethodPost, p.site+"/token/device/code", nil, struct{}{}, &device)
	if err == nil && (status != http.StatusOK || device.Token == "" || device.Code == "" || device.URL == "" || device.Life <= 0) {
		err = fmt.Errorf("%s, without a code", http.StatusText(status))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.actions != action {
		return nil // a sign-in or sign-out since then stands, not this one
	}
	if err != nil {
		p.problem = "TF1 gave no code to sign in with. telesfor's log has the reason."
		return fmt.Errorf("tf1: asking for a sign-in code: %w", err)
	}
	if p.pending != nil {
		p.pending.cancel()
	}
	poll := seconds(device.Interval)
	if poll <= 0 {
		poll = 5 * time.Second // as RFC 8628 has it for a device that is not told
	}
	// The wait outlasts the request that started it.
	waiting, cancel := context.WithTimeout(context.Background(), seconds(device.Life))
	wait := &pending{code: device.Code, url: cmp.Or(device.Complete, device.URL), expires: time.Now().Add(seconds(device.Life)), cancel: cancel}
	p.pending, p.problem = wait, ""
	go p.await(waiting, wait, device.Token, poll)
	return nil
}

// await waits for the user to enter the code, then takes up their account.
func (p *Provider) await(ctx context.Context, wait *pending, token string, poll time.Duration) {
	defer wait.cancel()
	signedIn, problem := p.authorize(ctx, token, poll)

	p.mu.Lock()
	if p.pending != wait { // given up, or a newer code took its place
		p.mu.Unlock()
		return
	}
	p.pending, p.problem = nil, problem
	if signedIn != nil {
		p.account, p.expired = signedIn, false
		if err := save(p.db, signedIn); err != nil {
			slog.Error("tf1: saving the sign-in", "err", err)
			p.problem = unsaved
		}
	}
	changed := p.changed
	p.mu.Unlock()

	if signedIn != nil && changed != nil {
		changed()
	}
}

// authorize asks TF1 about a code until the user has entered it, or its time
// is up. It returns the account, or what went wrong in words for the user.
func (p *Provider) authorize(ctx context.Context, token string, poll time.Duration) (*account, string) {
	for {
		var granted grant
		status, err := p.call(ctx, http.MethodPost, p.site+"/token/oauth2", nil,
			map[string]string{"grant_type": deviceGrant, "device_code": token}, &granted)
		signedIn := granted.account()
		switch {
		case err != nil:
			// The network's fault, or the code's time is up: the wait below tells.
		case status == http.StatusOK && signedIn != nil:
			return signedIn, ""
		case granted.Error == "authorization_pending":
			// The user has yet to enter the code.
		case granted.Error == "slow_down":
			poll += p.slower
		case granted.Error == "expired_token":
			return nil, codeExpired
		case granted.Error == "access_denied":
			return nil, "The sign-in was turned down on TF1's site."
		case status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
			// TF1 is busy, which says nothing about the code.
		default:
			slog.Error("tf1: signing in", "status", status, "err", granted.Error)
			return nil, "TF1 refused the sign-in: " + http.StatusText(status) + "."
		}
		select {
		case <-ctx.Done():
			return nil, codeExpired
		case <-time.After(poll):
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
		return fmt.Errorf("tf1: forgetting the sign-in: %w", err)
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

// token returns the account's access token, or none without an account. A
// token that is about to run out is renewed first, and so is the one that
// refused names: the one TF1 would not take.
func (p *Provider) token(ctx context.Context, refused string) (string, error) {
	// One at a time: a refresh token gets new tokens once.
	p.renewing.Lock()
	defer p.renewing.Unlock()

	p.mu.Lock()
	signedIn := p.account
	p.mu.Unlock()
	if signedIn == nil {
		return "", nil
	}
	left := time.Until(signedIn.Expires)
	if signedIn.Access != refused && (signedIn.Expires.IsZero() || left > margin) {
		return signedIn.Access, nil
	}

	// To the end, even if the viewer who asked has left meanwhile: TF1 takes
	// the refresh token once, so an answer that went unheard would be a
	// sign-in lost. The client's timeout bounds the wait.
	var granted grant
	status, err := p.call(context.WithoutCancel(ctx), http.MethodPost, p.site+"/token/oauth2", nil,
		map[string]string{"grant_type": "refresh_token", "refresh_token": signedIn.Refresh}, &granted)
	renewed := granted.account()

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.account == nil: // signed out since
		return "", nil
	case p.account != signedIn: // another account has taken its place since
		return p.account.Access, nil
	case err == nil && status == http.StatusOK && renewed != nil:
		p.account, p.expired = renewed, false
		// Before it is used: the tokens it was got with are good no more.
		if err := save(p.db, renewed); err != nil {
			slog.Error("tf1: saving the sign-in", "err", err)
			p.problem = unsaved
		}
		return renewed.Access, nil
	case err == nil && (status == http.StatusUnauthorized || status == http.StatusBadRequest):
		slog.Error("tf1: renewing the sign-in", "status", status, "err", granted.Error)
		p.expired = true
		return "", errExpired
	case signedIn.Access != refused && left > 0:
		// TF1 is out of reach, or busy: the token has time left.
		return signedIn.Access, nil
	case err != nil:
		return "", fmt.Errorf("renewing the sign-in: %w", err)
	}
	return "", fmt.Errorf("renewing the sign-in: TF1 answers %s", strings.TrimSpace(http.StatusText(status)+" "+granted.Error))
}

// expire notes that TF1 no longer accepts the account with the token, unless
// its tokens were renewed or another account has taken its place since.
func (p *Provider) expire(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.account != nil && p.account.Access == token {
		p.expired = true
	}
}

var (
	bucket     = []byte("tf1")
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
		return nil, fmt.Errorf("tf1: loading the sign-in from %s: %w", db.Path(), err)
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
