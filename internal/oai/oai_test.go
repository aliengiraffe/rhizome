package oai

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu          sync.Mutex
	items       map[string]string
	unavailable bool
	setHook     func()
}

func (s *memoryStore) Get(_ context.Context, k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return "", errStore
	}
	v, ok := s.items[k]
	if !ok {
		return "", errMissing
	}
	return v, nil
}
func (s *memoryStore) Set(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return errStore
	}
	s.items[k] = v
	if s.setHook != nil {
		s.setHook()
	}
	return nil
}
func (s *memoryStore) Delete(_ context.Context, k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return errStore
	}
	delete(s.items, k)
	return nil
}
func memory() *memoryStore { return &memoryStore{items: map[string]string{}} }
func encode(v any) string  { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }

type fixture struct {
	t                     *testing.T
	c                     *Client
	server                *httptest.Server
	key                   *rsa.PrivateKey
	mu                    sync.Mutex
	nonce                 string
	tokenCalls, jwksCalls atomic.Int32
	tokenStatus           int
	refreshSubject        string
	badJWKS               bool
	revokeStatus          int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, key: key}
	f.c = New(filepath.Join(t.TempDir(), "oai"))
	f.c.store = memory()
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.c.issuer = f.server.URL
	f.c.api = f.server.URL + "/v1"
	return f
}
func (f *fixture) claims(nonce, subject string) map[string]any {
	return map[string]any{"iss": f.c.issuer, "sub": subject, "aud": "issued_client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "email": "test@example.invalid"}
}
func (f *fixture) sign(claims map[string]any) string {
	header := encode(map[string]string{"alg": "RS256", "kid": "key"})
	payload := header + "." + encode(claims)
	digest := sha256.Sum256([]byte(payload))
	sig, e := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if e != nil {
		f.t.Fatal(e)
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		json.NewEncoder(w).Encode(discovery{Issuer: f.c.issuer, Authorize: f.server.URL + "/authorize", Token: f.server.URL + "/token", JWKS: f.server.URL + "/jwks", Revoke: f.server.URL + "/revoke", Algorithms: []string{"RS256", "ES256"}})
	case "/jwks":
		f.jwksCalls.Add(1)
		if f.badJWKS {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": []jwk{{Kty: "RSA", Kid: "key", Alg: "RS256", N: base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes())}}})
	case "/token":
		f.tokenCalls.Add(1)
		r.ParseForm()
		if r.Form.Get("resource") != resource || r.Form.Get("client_id") != "issued_client" {
			f.t.Error("invalid token form")
		}
		if r.Form.Get("grant_type") == "authorization_code" {
			b, e := os.ReadFile(filepath.Join(f.c.dir, "state.json"))
			if e != nil || !strings.Contains(string(b), "issued_client") {
				f.t.Error("client ID was not persisted before exchange")
			}
		}
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		subject := "subject"
		if r.Form.Get("grant_type") == "refresh_token" && f.refreshSubject != "" {
			subject = f.refreshSubject
		}
		reply := map[string]any{"access_token": "synthetic-access", "refresh_token": "synthetic-refresh-rotated", "id_token": f.sign(f.claims(f.nonce, subject)), "token_type": "Bearer", "expires_in": 3600}
		if r.Form.Get("grant_type") == "authorization_code" {
			reply["scope"] = scopes
			reply["refresh_token"] = "synthetic-refresh-initial"
		} else if r.Form.Get("refresh_token") != "synthetic-refresh-initial" {
			f.t.Error("refresh did not use the saved token")
		}
		json.NewEncoder(w).Encode(reply)
	case "/revoke":
		r.ParseForm()
		if r.Form.Get("token") == "" || r.Form.Get("client_id") != "issued_client" || r.Form.Get("token_type_hint") != "refresh_token" {
			f.t.Error("invalid revoke form")
		}
		if f.revokeStatus != 0 {
			w.WriteHeader(f.revokeStatus)
		}
	case "/v1/models":
		if r.Header.Get("Authorization") != "Bearer synthetic-access" {
			f.t.Error("missing bearer")
		}
		io.WriteString(w, `{"models":[{"slug":"hidden","visibility":"hide"},{"slug":"first","visibility":"list"},{"slug":"second","visibility":"list"}]}`)
	case "/v1/responses":
		if r.Header.Get("Authorization") != "Bearer synthetic-access" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("OpenAI-Organization") != "" || r.Header.Get("OpenAI-Project") != "" {
			f.t.Error("wrong inference headers")
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["store"] != false || b["stream"] != true || b["input"] == nil {
			f.t.Error("wrong inference body")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	default:
		w.WriteHeader(404)
	}
}
func (f *fixture) open(raw string) error {
	u, _ := url.Parse(raw)
	q := u.Query()
	f.mu.Lock()
	f.nonce = q.Get("nonce")
	f.mu.Unlock()
	if q.Get("ext_agent_host_id") == "" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != resource {
		f.t.Error("wrong authorize parameters")
	}
	cb, _ := url.Parse(q.Get("redirect_uri"))
	v := url.Values{"code": {"synthetic-code"}, "state": {q.Get("state")}, "client_id": {"issued_client"}}
	cb.RawQuery = v.Encode()
	resp, e := http.Get(cb.String())
	if e != nil {
		return e
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		f.t.Errorf("callback status %d", resp.StatusCode)
	}
	return nil
}
func (f *fixture) signin() {
	f.t.Helper()
	s, e := f.c.SignIn(context.Background(), f.open)
	if e != nil {
		f.t.Fatal(e)
	}
	if !s.SignedIn() || !s.PlanUsage() {
		f.t.Fatal("not signed in with usage")
	}
}
func TestSignInInferenceLogout(t *testing.T) {
	f := newFixture(t)
	f.signin()
	ctx := context.Background()
	s, e := f.c.Status()
	if e != nil || s.Account != "test@example.invalid" {
		t.Fatalf("status: %v %v", s, e)
	}
	text, e := f.c.Test(ctx, "", "hello")
	if e != nil || text != "hello" {
		t.Fatalf("test: %q %v", text, e)
	}
	filepath.Walk(f.c.dir, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			t.Fatal(e)
		}
		if !info.IsDir() {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "synthetic-access") || strings.Contains(string(b), "synthetic-refresh") || strings.Contains(string(b), "eyJ") {
				t.Error("secret written to disk")
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Errorf("mode %o", info.Mode().Perm())
			}
		}
		return nil
	})
	confirmed, e := f.c.Logout(ctx)
	if e != nil || !confirmed {
		t.Fatalf("logout: %v %v", confirmed, e)
	}
	s, _ = f.c.Status()
	if s.ClientID != "issued_client" || s.SignedIn() {
		t.Fatal("registration lost or tokens retained")
	}
	if _, e = f.c.Test(ctx, "first", "hello"); !errors.Is(e, errSignIn) {
		t.Fatalf("after logout: %v", e)
	}
}
func TestCallbackValidation(t *testing.T) {
	valid := url.Values{"state": {"expected"}, "code": {"code"}, "client_id": {"issued_client"}}
	tests := []struct {
		name, method, host, path, client string
		change                           func(url.Values)
		ok                               bool
		terminal                         bool
	}{
		{name: "valid", ok: true, terminal: true}, {name: "wrong state", change: func(q url.Values) { q.Set("state", "wrong") }},
		{name: "duplicate state", change: func(q url.Values) { q.Add("state", "expected") }},
		{name: "missing state", change: func(q url.Values) { q.Del("state") }},
		{name: "method", method: "POST"}, {name: "host", host: "localhost:1234"}, {name: "path", path: "/wrong"},
		{name: "duplicate code", change: func(q url.Values) { q.Add("code", "code") }},
		{name: "missing code", change: func(q url.Values) { q.Del("code") }},
		{name: "empty code", change: func(q url.Values) { q.Set("code", "") }},
		{name: "dynamic client", change: func(q url.Values) { q.Set("client_id", "dynamic_agent_client") }},
		{name: "invalid client", change: func(q url.Values) { q.Set("client_id", "../../bad") }},
		{name: "long client", change: func(q url.Values) { q.Set("client_id", strings.Repeat("x", 201)) }},
		{name: "duplicate client", change: func(q url.Values) { q.Add("client_id", "issued_client") }},
		{name: "missing new client", change: func(q url.Values) { q.Del("client_id") }},
		{name: "mismatched client", client: "saved_client"},
		{name: "returning missing client", client: "issued_client", change: func(q url.Values) { q.Del("client_id") }, ok: true, terminal: true},
		{name: "error", change: func(q url.Values) { q.Del("code"); q.Set("error", "access_denied") }, ok: true, terminal: true},
		{name: "duplicate error", change: func(q url.Values) { q.Del("code"); q.Add("error", "denied"); q.Add("error", "denied") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range valid {
				q[k] = append([]string{}, v...)
			}
			if tt.change != nil {
				tt.change(q)
			}
			method, host, path := tt.method, tt.host, tt.path
			if method == "" {
				method = "GET"
			}
			if host == "" {
				host = "127.0.0.1:1234"
			}
			if path == "" {
				path = "/auth/callback"
			}
			ch := make(chan callbackResult, 1)
			handler := callbackHandler("127.0.0.1:1234", "expected", tt.client, ch)
			r := httptest.NewRequest(method, "http://"+host+path+"?"+q.Encode(), nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if (w.Code == 200) != tt.ok {
				t.Fatalf("status %d", w.Code)
			}
			if (len(ch) == 1) != tt.terminal {
				t.Fatal("invalid callback ended attempt")
			}
		})
	}
	// A rejected state must leave this exact pending handler usable.
	ch := make(chan callbackResult, 1)
	h := callbackHandler("127.0.0.1:1234", "expected", "", ch)
	for _, state := range []string{"wrong", "expected"} {
		q := url.Values{"state": {state}, "code": {"code"}, "client_id": {"issued_client"}}
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://127.0.0.1:1234/auth/callback?"+q.Encode(), nil))
	}
	if len(ch) != 1 {
		t.Fatal("attempt did not survive invalid state")
	}
}
func TestPKCE(t *testing.T) {
	if got := challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal(got)
	}
	if len(randomValue()) != 43 || randomValue() == randomValue() {
		t.Fatal("random values")
	}
}
func TestIdentity(t *testing.T) {
	f := newFixture(t)
	d, e := f.c.discover(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"valid", "signature", "issuer", "audience", "nonce", "expired", "unknown kid", "none", "multi audience", "azp", "future nbf"} {
		t.Run(kind, func(t *testing.T) {
			claims := f.claims("nonce", "subject")
			switch kind {
			case "issuer":
				claims["iss"] = "wrong"
			case "audience":
				claims["aud"] = "wrong"
			case "nonce":
				claims["nonce"] = "wrong"
			case "expired":
				claims["exp"] = int64(1)
			case "multi audience":
				claims["aud"] = []string{"issued_client", "other"}
			case "azp":
				claims["azp"] = "other"
			case "future nbf":
				claims["nbf"] = time.Now().Add(time.Hour).Unix()
			}
			token := f.sign(claims)
			p := strings.Split(token, ".")
			switch kind {
			case "signature":
				p[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
			case "unknown kid":
				p[0] = encode(map[string]string{"alg": "RS256", "kid": "other"})
			case "none":
				p[0] = encode(map[string]string{"alg": "none", "kid": "key"})
			}
			_, e := f.c.verify(context.Background(), d, strings.Join(p, "."), "issued_client", "nonce")
			if (e == nil) != (kind == "valid") {
				t.Fatalf("verification: %v", e)
			}
		})
	}
}
func TestES256(t *testing.T) {
	f := newFixture(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []jwk{{Kid: "ec", Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}})
	}))
	defer s.Close()
	payload := encode(map[string]string{"alg": "ES256", "kid": "ec"}) + "." + encode(f.claims("nonce", "subject"))
	hash := sha256.Sum256([]byte(payload))
	r, ss, _ := ecdsa.Sign(rand.Reader, key, hash[:])
	sig := append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
	_, e := f.c.verify(context.Background(), discovery{JWKS: s.URL, Algorithms: []string{"ES256"}}, payload+"."+base64.RawURLEncoding.EncodeToString(sig), "issued_client", "nonce")
	if e != nil {
		t.Fatal(e)
	}
}
func TestTokenValidation(t *testing.T) {
	scope := scopes
	for _, kind := range []string{"valid", "access", "refresh", "id", "scope", "type", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			v := tokenResponse{Access: "a", Refresh: "r", ID: "i", Type: "Bearer", Expires: 3600, Scope: &scope}
			switch kind {
			case "access":
				v.Access = ""
			case "refresh":
				v.Refresh = ""
			case "id":
				v.ID = ""
			case "scope":
				v.Scope = nil
			case "type":
				v.Type = "Basic"
			case "expiry":
				v.Expires = 0
			}
			if (v.validate(true) == nil) != (kind == "valid") {
				t.Fatal("validation")
			}
		})
	}
}
func TestRegistrationSurvivesFailure(t *testing.T) {
	f := newFixture(t)
	f.tokenStatus = 400
	_, e := f.c.SignIn(context.Background(), f.open)
	if e == nil {
		t.Fatal("expected failure")
	}
	s, _ := f.c.Status()
	if s.ClientID != "issued_client" || s.SignedIn() {
		t.Fatal("registration not retained")
	}
	f.mu.Lock()
	f.tokenStatus = 0
	f.mu.Unlock()
	_, e = f.c.SignIn(context.Background(), func(raw string) error {
		u, _ := url.Parse(raw)
		if u.Query().Get("client_id") != "issued_client" || u.Query().Get("agent_name_hint") != "" {
			t.Error("registration was not reused")
		}
		return f.open(raw)
	})
	if e != nil {
		t.Fatal(e)
	}
}
func expire(t *testing.T, f *fixture, earliest time.Time) {
	t.Helper()
	v, e := openVault(f.c.dir, f.c.store, false)
	if e != nil {
		t.Fatal(e)
	}
	defer v.close()
	v.state.Expires = time.Now().Add(30 * time.Second)
	v.state.Earliest = earliest
	if e = v.save(); e != nil {
		t.Fatal(e)
	}
}
func TestRefreshPendingRecovery(t *testing.T) {
	f := newFixture(t)
	f.signin()
	expire(t, f, time.Time{})
	f.mu.Lock()
	f.badJWKS = true
	f.mu.Unlock()
	_, e := f.c.Test(context.Background(), "first", "hello")
	if e == nil {
		t.Fatal("expected JWKS failure")
	}
	s, _ := f.c.Status()
	if !s.Pending || s.Refreshing || s.SignedIn() {
		t.Fatal("rotation not saved as pending")
	}
	calls := f.tokenCalls.Load()
	v, _ := openVault(f.c.dir, f.c.store, false)
	creds, e := v.load(context.Background())
	v.close()
	if e != nil || creds.Refresh != "synthetic-refresh-rotated" {
		t.Fatal("rotated token not saved")
	}
	f.mu.Lock()
	f.badJWKS = false
	f.mu.Unlock()
	_, e = f.c.Test(context.Background(), "first", "hello")
	if e != nil {
		t.Fatal(e)
	}
	if f.tokenCalls.Load() != calls {
		t.Fatal("consumed refresh token reused")
	}
}
func TestRefreshMismatchAndTiming(t *testing.T) {
	for _, kind := range []string{"mismatch", "earliest", "fresh", "invalid grant"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.signin()
			switch kind {
			case "mismatch":
				expire(t, f, time.Time{})
				f.mu.Lock()
				f.refreshSubject = "other"
				f.mu.Unlock()
			case "earliest":
				expire(t, f, time.Now().Add(time.Hour))
			case "invalid grant":
				expire(t, f, time.Time{})
				f.mu.Lock()
				f.tokenStatus = 400
				f.mu.Unlock()
			}
			_, e := f.c.Test(context.Background(), "first", "hello")
			if kind == "mismatch" {
				if e == nil || !strings.Contains(e.Error(), "account_mismatch") {
					t.Fatalf("got %v", e)
				}
				s, _ := f.c.Status()
				if !s.Pending {
					t.Fatal("mismatch became usable")
				}
			} else if kind == "invalid grant" {
				if e == nil {
					t.Fatal("expected invalid grant")
				}
				s, _ := f.c.Status()
				if s.Secrets != nil {
					t.Fatal("unusable credentials retained")
				}
			} else if e != nil || f.tokenCalls.Load() != 1 {
				t.Fatalf("unexpected refresh %v", e)
			}
		})
	}
}
func TestRevocationFailure(t *testing.T) {
	f := newFixture(t)
	f.signin()
	f.mu.Lock()
	f.revokeStatus = 503
	f.mu.Unlock()
	ok, e := f.c.Logout(context.Background())
	if e != nil || ok {
		t.Fatalf("%v %v", ok, e)
	}
	s, _ := f.c.Status()
	if s.Secrets != nil || s.ClientID == "" {
		t.Fatal("logout state")
	}
}
func TestStream(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		ok                bool
	}{
		{"success", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\"}\n\n", "hello", true},
		{"multiline", "data: {\"type\":\"response.output_text.delta\",\n data: ignored\ndata: \"delta\":\"hi\"}\n\ndata: {\"type\":\"response.completed\"}\n\n", "hi", true},
		{"interrupted", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "", false},
		{"incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", "", false},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}}\n\n", "", false},
		{"oversized", strings.Repeat("x", 300<<10), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, e := readStream(strings.NewReader(tt.input), "req-id")
			if (e == nil) != tt.ok || got != tt.want {
				t.Fatalf("%q %v", got, e)
			}
			if tt.name == "failed" && !strings.Contains(e.Error(), "https://chatgpt.com/settings/usage") {
				t.Fatal(e)
			}
		})
	}
}
func TestHTTPFailures(t *testing.T) {
	for _, kind := range []string{"redirect", "429", "large", "issuer"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch kind {
				case "redirect":
					http.Redirect(w, r, "/other", 302)
				case "429":
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`)
				case "large":
					io.WriteString(w, strings.Repeat("x", (1<<20)+1))
				case "issuer":
					io.WriteString(w, `{"issuer":"wrong"}`)
				}
			}))
			defer s.Close()
			c := New(t.TempDir())
			c.issuer = s.URL
			_, e := c.discover(context.Background())
			if e == nil || calls.Load() != 1 {
				t.Fatalf("error=%v calls=%d", e, calls.Load())
			}
		})
	}
}
func TestStoreFailureLockAndAtomicity(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "oai")
	m := memory()
	v, e := openVault(dir, m, false)
	if e != nil {
		t.Fatal(e)
	}
	defer v.close()
	if _, e = openVault(dir, m, false); e == nil {
		t.Fatal("lock contention accepted")
	}
	v.state.HostID = hostID()
	if e = v.save(); e != nil {
		t.Fatal(e)
	}
	if e = v.install(ctx, credentials{Access: strings.Repeat("secret", 1000), Refresh: "refresh"}, v.state); e != nil {
		t.Fatal(e)
	}
	old := *v.state.Secrets
	creds, e := v.load(ctx)
	if e != nil || len(creds.Access) != 6000 {
		t.Fatal("chunked read")
	}
	m.unavailable = true
	if e = v.install(ctx, credentials{Access: "replacement", Refresh: "replacement-refresh"}, v.state); e == nil {
		t.Fatal("store failure accepted")
	}
	if *v.state.Secrets != old {
		t.Fatal("active pointer replaced on failure")
	}
	m.unavailable = false
	if e = v.clear(ctx); e != nil {
		t.Fatal(e)
	}
	if len(m.items) != 0 {
		t.Fatal("orphan secrets not deleted")
	}
}
func TestUnavailableStoreBeforeBrowser(t *testing.T) {
	f := newFixture(t)
	f.c.store.(*memoryStore).unavailable = true
	_, e := f.c.SignIn(context.Background(), func(string) error { t.Fatal("browser opened without secure storage"); return nil })
	if !errors.Is(e, errStore) {
		t.Fatal(e)
	}
}
func TestCallbackCancellation(t *testing.T) {
	ln, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	_, e = awaitCallback(ctx, ln, "state", "", func(string) error { cancel(); return nil })
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	again, e := net.Listen("tcp4", addr)
	if e != nil {
		t.Fatalf("listener leaked: %v", e)
	}
	again.Close()
}
func TestErrorsNeverEchoBody(t *testing.T) {
	for _, code := range []string{"subscription_sharing_usage_limit_exceeded", "subscription_sharing_v2_client_not_enabled", "invalid_grant", "unknown-secret"} {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": "synthetic-secret"}})
		e := remoteError(429, "req-123", body)
		if strings.Contains(e.Error(), "synthetic-secret") || strings.Contains(e.Error(), "unknown-secret") || !strings.Contains(e.Error(), "req-123") {
			t.Fatal(e)
		}
	}
}

func TestRefreshStorageFailureFencesOldToken(t *testing.T) {
	f := newFixture(t)
	f.signin()
	expire(t, f, time.Time{})
	m := f.c.store.(*memoryStore)
	m.mu.Lock()
	m.unavailable = true
	m.mu.Unlock()
	// Failure to read cannot consume the old token.
	_, e := f.c.Test(context.Background(), "first", "hi")
	if e == nil {
		t.Fatal("expected store error")
	}
	if f.tokenCalls.Load() != 1 {
		t.Fatal("refresh without readable credentials")
	}
	m.mu.Lock()
	m.unavailable = false
	m.mu.Unlock()
	// Simulate interruption after a refresh was sent but before replacement persisted.
	v, e := openVault(f.c.dir, m, false)
	if e != nil {
		t.Fatal(e)
	}
	v.state.Refreshing = true
	if e = v.save(); e != nil {
		t.Fatal(e)
	}
	v.close()
	_, e = f.c.Test(context.Background(), "first", "hi")
	if e == nil || !strings.Contains(e.Error(), "uncertain") {
		t.Fatalf("%v", e)
	}
	if f.tokenCalls.Load() != 1 {
		t.Fatal("uncertain token reused")
	}
}
func TestMissingPlanPermission(t *testing.T) {
	f := newFixture(t)
	f.signin()
	v, e := openVault(f.c.dir, f.c.store, false)
	if e != nil {
		t.Fatal(e)
	}
	v.state.Scopes = []string{"openid"}
	if e = v.save(); e != nil {
		t.Fatal(e)
	}
	v.close()
	_, e = f.c.Test(context.Background(), "first", "hi")
	if e == nil || !strings.Contains(e.Error(), "not granted") {
		t.Fatalf("%v", e)
	}
}
func TestStatePermissionsAndStatusSnapshot(t *testing.T) {
	f := newFixture(t)
	f.signin()
	v, e := openVault(f.c.dir, f.c.store, false)
	if e != nil {
		t.Fatal(e)
	}
	defer v.close()
	before, e := os.ReadFile(filepath.Join(f.c.dir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	// Read-only status does not acquire the writer lock or read the secret store.
	f.c.store.(*memoryStore).unavailable = true
	s, e := f.c.Status()
	if e != nil || !s.SignedIn() {
		t.Fatalf("status %v %v", s, e)
	}
	after, _ := os.ReadFile(filepath.Join(f.c.dir, "state.json"))
	if string(before) != string(after) {
		t.Fatal("status rewrote state")
	}
	if runtime.GOOS != "windows" {
		if e = os.Chmod(filepath.Join(f.c.dir, "state.json"), 0644); e != nil {
			t.Fatal(e)
		}
		if _, e = f.c.Status(); e == nil {
			t.Fatal("public state accepted")
		}
	}
}

func TestTokenMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	old := State{Scopes: []string{"openid", "chatgpt.tokens.use.direct"}}
	for _, raw := range []json.RawMessage{json.RawMessage([]byte(big.NewInt(now.Add(time.Minute).Unix()).String())), json.RawMessage(`"` + now.Add(time.Minute).Format(time.RFC3339) + `"`)} {
		s, e := (tokenResponse{Expires: 3600, Earliest: raw}).state(old, now)
		if e != nil || !s.PlanUsage() || !s.Earliest.Equal(now.Add(time.Minute)) || !s.Expires.Equal(now.Add(time.Hour)) {
			t.Fatalf("metadata %v %v", s, e)
		}
	}
	empty := ""
	s, e := (tokenResponse{Expires: 3600, Scope: &empty}).state(old, now)
	if e != nil || s.PlanUsage() {
		t.Fatal("explicit empty grant retained usage")
	}
	if _, e = (tokenResponse{Expires: 3600, Earliest: json.RawMessage(`{}`)}).state(old, now); e == nil {
		t.Fatal("invalid earliest_refresh_at accepted")
	}
}
