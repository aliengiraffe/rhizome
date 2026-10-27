package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Repository is the only release source the updater reads.
const Repository = "aliengiraffe/rhizome"

// Default endpoints and bounds.
const (
	DefaultAPI       = "https://api.github.com"
	DefaultDownloads = "https://github.com"
	maxReleaseList   = 1 << 20 // API response bound
	maxRedirects     = 5       // per request
	apiTimeout       = 30 * time.Second
	downloadTimeout  = 15 * time.Minute
	releasesPerPage  = 30
)

// githubHosts are the hosts a redirect may lead to; anything else is refused.
var githubHosts = map[string]bool{"api.github.com": true, "github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true}

var hexSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Release is one published GitHub release as the updater sees it.
type Release struct {
	Tag        string
	Prerelease bool
	Draft      bool
}

// Source lists releases and streams release assets.
type Source interface {
	// Releases returns published releases, newest first as the API orders them.
	Releases(ctx context.Context) ([]Release, error)
	// TagCommit resolves the commit a release tag names.
	TagCommit(ctx context.Context, tag string) (string, error)
	// Download streams asset name of tag into w, failing once limit bytes are
	// exceeded, and returns the byte count.
	Download(ctx context.Context, tag, name string, limit int64, w io.Writer) (int64, error)
}

// Error carries a stable outcome code for logs and status.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func coded(code string, err error) error {
	var existing *Error
	if errors.As(err, &existing) {
		return err
	}
	return &Error{Code: code, Err: err}
}

// Code extracts the outcome code of err, or a generic one.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return "error"
}

// GitHub reads releases of Repository through the REST API and downloads
// assets from the release download path it constructs itself; URLs supplied
// by the API or release bodies are never followed.
type GitHub struct {
	api, downloads *url.URL
	client         *http.Client
	userAgent      string
}

// NewGitHub returns a source for the given endpoints. Non-HTTPS endpoints are
// accepted only on loopback, for local fakes. client may be nil.
func NewGitHub(api, downloads, userAgent string, client *http.Client) (*GitHub, error) {
	a, err := parseBase(api)
	if err != nil {
		return nil, fmt.Errorf("update source api %q: %w", api, err)
	}
	d, err := parseBase(downloads)
	if err != nil {
		return nil, fmt.Errorf("update source downloads %q: %w", downloads, err)
	}
	g := &GitHub{api: a, downloads: d, userAgent: userAgent}
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second, ForceAttemptHTTP2: true, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext}}
	}
	// Copy so the redirect policy never leaks into a caller's client.
	c := *client
	allowed := map[string]bool{a.Host: true, d.Host: true}
	for h := range githubHosts {
		allowed[h] = true
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return coded("redirect_refused", errors.New("too many redirects"))
		}
		if err := checkURL(req.URL, allowed); err != nil {
			return coded("redirect_refused", err)
		}
		return nil
	}
	g.client = &c
	return g, nil
}

func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must be a scheme and host without credentials, query or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if err := checkURL(u, map[string]bool{u.Host: true}); err != nil {
		return nil, err
	}
	return u, nil
}

// checkURL requires HTTPS (plain HTTP only on loopback) and an allowed host.
func checkURL(u *url.URL, allowed map[string]bool) error {
	host := u.Hostname()
	loopback := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback {
			return fmt.Errorf("refusing plain http to %s", host)
		}
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if !allowed[u.Host] {
		return fmt.Errorf("host %s is not a release host", u.Host)
	}
	return nil
}

func (g *GitHub) get(ctx context.Context, u *url.URL, accept string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, coded("invalid_request", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", g.userAgent)
	if accept == "application/vnd.github+json" {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, coded("timeout", err)
		}
		var ce *Error
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, coded("network_error", err)
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		resp.Body.Close()
		return nil, coded("rate_limited", fmt.Errorf("status %d", resp.StatusCode))
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, coded("not_found", fmt.Errorf("status %d", resp.StatusCode))
	}
	resp.Body.Close()
	return nil, coded("http_error", fmt.Errorf("status %d", resp.StatusCode))
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error { c.cancel(); return c.ReadCloser.Close() }

func (g *GitHub) apiJSON(ctx context.Context, path, query string, out any) error {
	u := *g.api
	u.Path = g.api.Path + path
	u.RawQuery = query
	resp, err := g.get(ctx, &u, "application/vnd.github+json", apiTimeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseList+1))
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return coded("timeout", err)
		}
		return coded("network_error", err)
	}
	if len(body) > maxReleaseList {
		return coded("oversize", errors.New("API response too large"))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return coded("invalid_release", err)
	}
	return nil
}

// Releases implements Source.
func (g *GitHub) Releases(ctx context.Context) ([]Release, error) {
	var raw []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	err := g.apiJSON(ctx, fmt.Sprintf("/repos/%s/releases", Repository), fmt.Sprintf("per_page=%d", releasesPerPage), &raw)
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(raw))
	for _, r := range raw {
		out = append(out, Release{Tag: r.TagName, Prerelease: r.Prerelease, Draft: r.Draft})
	}
	return out, nil
}

// TagCommit implements Source. Annotated tags are dereferenced once.
func (g *GitHub) TagCommit(ctx context.Context, tag string) (string, error) {
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.apiJSON(ctx, fmt.Sprintf("/repos/%s/git/ref/tags/%s", Repository, url.PathEscape(tag)), "", &ref); err != nil {
		return "", err
	}
	if !hexSHA.MatchString(ref.Object.SHA) {
		return "", coded("invalid_release", errors.New("tag ref without a commit sha"))
	}
	if ref.Object.Type == "commit" {
		return ref.Object.SHA, nil
	}
	if ref.Object.Type != "tag" {
		return "", coded("invalid_release", fmt.Errorf("tag ref of type %q", ref.Object.Type))
	}
	var obj struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.apiJSON(ctx, fmt.Sprintf("/repos/%s/git/tags/%s", Repository, ref.Object.SHA), "", &obj); err != nil {
		return "", err
	}
	if obj.Object.Type != "commit" || !hexSHA.MatchString(obj.Object.SHA) {
		return "", coded("invalid_release", errors.New("annotated tag does not name a commit"))
	}
	return obj.Object.SHA, nil
}

// Download implements Source.
func (g *GitHub) Download(ctx context.Context, tag, name string, limit int64, w io.Writer) (int64, error) {
	if strings.ContainsAny(name, "/\\") || strings.ContainsAny(tag, "/\\") {
		return 0, coded("invalid_request", errors.New("asset and tag names must be plain"))
	}
	u := *g.downloads
	u.Path = fmt.Sprintf("%s/%s/releases/download/%s/%s", g.downloads.Path, Repository, tag, name)
	resp, err := g.get(ctx, &u, "application/octet-stream", downloadTimeout)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return 0, coded("oversize", fmt.Errorf("%s announces %d bytes, limit %d", name, resp.ContentLength, limit))
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return n, coded("timeout", err)
		}
		return n, coded("network_error", err)
	}
	if n > limit {
		return n, coded("oversize", fmt.Errorf("%s exceeds %d bytes", name, limit))
	}
	return n, nil
}
