package oai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"
)

var safeLabel = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)
var clientPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,200}$`)

func validClientID(s string) bool { return s != "dynamic_agent_client" && clientPattern.MatchString(s) }
func randomValue() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func challenge(s string) string {
	b := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(b[:])
}

type callbackResult struct {
	code, client string
	err          error
}

func callbackHandler(host, state, client string, results chan<- callbackResult) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		reject := func() { http.Error(w, "Invalid sign-in callback", 400) }
		if r.Method != "GET" || r.Host != host || r.URL.Path != "/auth/callback" || r.URL.RawPath != "" || len(r.URL.RawQuery) > 16<<10 {
			reject()
			return
		}
		q, e := url.ParseQuery(r.URL.RawQuery)
		if e != nil || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			reject()
			return
		}
		if len(q["error"]) > 0 {
			if len(q["error"]) != 1 || q.Get("error") == "" || len(q["code"]) > 0 {
				reject()
				return
			}
			once.Do(func() {
				results <- callbackResult{err: errors.New("authorization declined or failed; run rhizome auth oai to try again")}
			})
		} else {
			returned := q.Get("client_id")
			if len(q["client_id"]) == 0 && client != "" {
				returned = client
			}
			if len(q["code"]) != 1 || q.Get("code") == "" || len(q.Get("code")) > 8192 || len(q["client_id"]) > 1 || !validClientID(returned) || (client != "" && returned != client) {
				reject()
				return
			}
			once.Do(func() { results <- callbackResult{code: q.Get("code"), client: returned} })
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<!doctype html><title>Rhizome</title><p>Return to your terminal to finish signing in.</p>")
	})
}

// awaitCallback owns and joins the HTTP server and any active handlers on every exit.
func awaitCallback(ctx context.Context, ln net.Listener, state, client string, start func(string) error) (callbackResult, error) {
	results := make(chan callbackResult, 1)
	server := &http.Server{Handler: callbackHandler(ln.Addr().String(), state, client, results), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 20 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ln) }()
	defer func() { server.Close(); <-done }()
	if e := start("http://" + ln.Addr().String() + "/auth/callback"); e != nil {
		return callbackResult{}, e
	}
	select {
	case r := <-results:
		return r, r.err
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	case <-done:
		return callbackResult{}, errors.New("sign-in listener stopped")
	}
}
