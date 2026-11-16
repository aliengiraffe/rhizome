package oai

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
)

type tokenResponse struct {
	Access   string          `json:"access_token"`
	Refresh  string          `json:"refresh_token"`
	ID       string          `json:"id_token"`
	Type     string          `json:"token_type"`
	Scope    *string         `json:"scope"`
	Expires  int64           `json:"expires_in"`
	Earliest json.RawMessage `json:"earliest_refresh_at"`
}

func (t tokenResponse) validate(initial bool) error {
	if !strings.EqualFold(t.Type, "bearer") || t.Access == "" || t.Refresh == "" || len(t.Access) > 32<<10 || len(t.Refresh) > 16<<10 || len(t.ID) > 64<<10 || t.Expires <= 0 || t.Expires > 86400 || (initial && (t.ID == "" || t.Scope == nil || *t.Scope == "")) {
		return errors.New("invalid token response; sign in again with rhizome auth oai")
	}
	if strings.ContainsAny(t.Access, "\r\n") {
		return errors.New("invalid access token")
	}
	return nil
}
func (t tokenResponse) state(old State, now time.Time) (State, error) {
	old.Expires = now.Add(time.Duration(t.Expires) * time.Second)
	old.Earliest = time.Time{}
	if len(t.Earliest) > 0 && string(t.Earliest) != "null" {
		var epoch int64
		var stamp string
		if json.Unmarshal(t.Earliest, &epoch) == nil && epoch > 0 {
			old.Earliest = time.Unix(epoch, 0).UTC()
		} else if json.Unmarshal(t.Earliest, &stamp) == nil {
			var e error
			old.Earliest, e = time.Parse(time.RFC3339, stamp)
			if e != nil {
				return old, errors.New("invalid earliest_refresh_at")
			}
		} else {
			return old, errors.New("invalid earliest_refresh_at")
		}
	}
	if t.Scope != nil {
		if len(*t.Scope) > 4096 {
			return old, errors.New("granted scope exceeds limit")
		}
		old.Scopes = strings.Fields(*t.Scope)
	}
	return old, nil
}
func (c *Client) exchange(ctx context.Context, d discovery, form url.Values) (tokenResponse, error) {
	var t tokenResponse
	e := c.jsonRequest(ctx, "POST", d.Token, "application/x-www-form-urlencoded", "", strings.NewReader(form.Encode()), &t)
	return t, e
}
func accountLabel(id identity) string {
	s := id.Email
	if s == "" {
		s = id.Name
	}
	if s == "" {
		s = "ChatGPT account"
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

// Status reads local metadata without contacting an issuer or loading any token.
func (c *Client) Status() (State, error) {
	v, e := openVault(c.dir, c.store, true)
	if e != nil {
		return State{}, e
	}
	defer v.close()
	return v.state, nil
}

// SignIn starts a five-minute, cancellable loopback sign-in. Open receives only
// an authorization URL without any saved token; callers may print it on failure.
func (c *Client) SignIn(ctx context.Context, open func(string) error) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	v, e := openVault(c.dir, c.store, false)
	if e != nil {
		return State{}, e
	}
	defer v.close()
	if e = v.cleanup(ctx); e != nil {
		return State{}, e
	}
	// Check the actual store before sending the user through browser consent.
	probe := secretRef{ID: randomID(), Chunks: 1}
	v.state.Garbage = append(v.state.Garbage, probe)
	if e = v.save(); e != nil {
		return State{}, e
	}
	if e = c.store.Set(ctx, chunkKey(probe, 0), "availability-check"); e != nil {
		return State{}, errStore
	}
	if e = v.cleanup(ctx); e != nil {
		return State{}, e
	}
	if v.state.HostID == "" {
		v.state.HostID = hostID()
		if e = v.save(); e != nil {
			return State{}, e
		}
	}
	d, e := c.discover(ctx)
	if e != nil {
		return State{}, e
	}
	ln, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return State{}, errors.New("cannot start local sign-in listener")
	}
	v.state.Port = ln.Addr().(*net.TCPAddr).Port
	state, nonce, verifier := randomValue(), randomValue(), randomValue()
	var redirect string
	result, e := awaitCallback(ctx, ln, state, v.state.ClientID, func(uri string) error {
		redirect = uri
		u, _ := url.Parse(d.Authorize)
		q := u.Query()
		id := v.state.ClientID
		if id == "" {
			id = "dynamic_agent_client"
			q.Set("agent_name_hint", "Rhizome")
		} else if !v.state.PlanUsage() {
			q.Set("prompt", "consent")
		}
		q.Set("client_id", id)
		q.Set("ext_agent_host_id", v.state.HostID)
		q.Set("response_type", "code")
		q.Set("redirect_uri", uri)
		q.Set("resource", resource)
		q.Set("scope", scopes)
		q.Set("state", state)
		q.Set("nonce", nonce)
		q.Set("code_challenge_method", "S256")
		q.Set("code_challenge", challenge(verifier))
		u.RawQuery = q.Encode()
		return open(u.String())
	})
	if e != nil {
		return State{}, e
	}
	// Preserve registration even when code exchange fails or is cancelled.
	v.state.ClientID = result.client
	if e = v.save(); e != nil {
		return State{}, e
	}
	t, e := c.exchange(ctx, d, url.Values{"grant_type": {"authorization_code"}, "client_id": {result.client}, "code": {result.code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {resource}})
	if e != nil {
		return State{}, e
	}
	if e = t.validate(true); e != nil {
		return State{}, e
	}
	id, e := c.verify(ctx, d, t.ID, result.client, nonce)
	if e != nil {
		return State{}, e
	}
	if v.state.Subject != "" && v.state.Subject != id.Subject {
		return State{}, errors.New("account_mismatch: sign in with the account registered on this device")
	}
	next, e := t.state(v.state, c.now())
	if e != nil {
		return State{}, e
	}
	next.Subject = id.Subject
	next.Account = accountLabel(id)
	next.Pending = false
	next.Refreshing = false
	if ctx.Err() != nil {
		return State{}, ctx.Err()
	}
	if e = v.install(ctx, credentials{Access: t.Access, Refresh: t.Refresh, ID: t.ID}, next); e != nil {
		return State{}, e
	}
	return v.state, nil
}
func (c *Client) access(ctx context.Context, v *vault) (credentials, error) {
	empty := credentials{}
	if v.state.Refreshing {
		return empty, errors.New("previous refresh outcome is uncertain; run rhizome auth oai before using credentials")
	}
	creds, e := v.load(ctx)
	if e != nil {
		return empty, e
	}
	if v.state.Pending {
		d, e := c.discover(ctx)
		if e != nil {
			return empty, e
		}
		if e = c.finishRefresh(ctx, v, d, creds); e != nil {
			return empty, e
		}
	}
	if !v.state.PlanUsage() {
		return empty, errors.New("ChatGPT plan usage was not granted; run rhizome auth oai to request consent")
	}
	now := c.now()
	if now.Add(60 * time.Second).Before(v.state.Expires) {
		return creds, nil
	}
	if now.Before(v.state.Earliest) {
		if now.Before(v.state.Expires) {
			return creds, nil
		}
		return empty, errors.New("token expired before allowed refresh time; retry after " + v.state.Earliest.UTC().Format(time.RFC3339))
	}
	d, e := c.discover(ctx)
	if e != nil {
		return empty, e
	}
	// A crash or storage failure after sending a rotating token must never reuse it.
	v.state.Refreshing = true
	if e = v.save(); e != nil {
		return empty, e
	}
	t, e := c.exchange(ctx, d, url.Values{"grant_type": {"refresh_token"}, "client_id": {v.state.ClientID}, "refresh_token": {creds.Refresh}, "resource": {resource}})
	if e != nil {
		if terminalRefresh(e) {
			if ce := v.clear(ctx); ce != nil {
				return empty, ce
			}
		}
		return empty, e
	}
	if e = t.validate(false); e != nil {
		return empty, e
	}
	next, e := t.state(v.state, now)
	if e != nil {
		return empty, e
	}
	next.Refreshing = false
	next.Pending = t.ID != ""
	refreshed := credentials{Access: t.Access, Refresh: t.Refresh, ID: t.ID}
	if refreshed.ID == "" {
		refreshed.ID = creds.ID
	}
	if e = v.install(ctx, refreshed, next); e != nil {
		return empty, e
	}
	if next.Pending {
		if e = c.finishRefresh(ctx, v, d, refreshed); e != nil {
			return empty, e
		}
	}
	if !v.state.PlanUsage() {
		return empty, errors.New("ChatGPT plan usage was not granted; run rhizome auth oai")
	}
	return refreshed, nil
}
func (c *Client) finishRefresh(ctx context.Context, v *vault, d discovery, creds credentials) error {
	id, e := c.verify(ctx, d, creds.ID, v.state.ClientID, "")
	if e != nil {
		return e
	}
	if id.Subject != v.state.Subject {
		return errors.New("account_mismatch: refreshed account differs; run rhizome auth oai")
	}
	v.state.Account = accountLabel(id)
	v.state.Pending = false
	return v.save()
}

// Logout attempts revocation, always removes local tokens, and keeps registration.
// The returned boolean reports whether remote revocation was confirmed.
func (c *Client) Logout(ctx context.Context) (bool, error) {
	v, e := openVault(c.dir, c.store, false)
	if e != nil {
		return false, e
	}
	defer v.close()
	confirmed := v.state.Secrets == nil
	if v.state.Secrets != nil {
		creds, e := v.load(ctx)
		if e == nil {
			d, e := c.discover(ctx)
			if e == nil {
				e = c.jsonRequest(ctx, "POST", d.Revoke, "application/x-www-form-urlencoded", "", strings.NewReader(url.Values{"token": {creds.Refresh}, "token_type_hint": {"refresh_token"}, "client_id": {v.state.ClientID}}.Encode()), nil)
				confirmed = e == nil
			}
		}
	}
	// Cancellation of network work must not prevent removing local credentials.
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e = v.clear(cleanup); e != nil {
		return confirmed, e
	}
	return confirmed, nil
}
