package oai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const issuer = "https://auth.openai.com"
const resource = "https://api.openai.com/v1"
const scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

type discovery struct {
	Issuer     string   `json:"issuer"`
	Authorize  string   `json:"authorization_endpoint"`
	Token      string   `json:"token_endpoint"`
	JWKS       string   `json:"jwks_uri"`
	Revoke     string   `json:"revocation_endpoint"`
	Algorithms []string `json:"id_token_signing_alg_values_supported"`
}

// Client owns explicit CLI operations; construction does no I/O.
type Client struct {
	dir         string
	store       SecretStore
	http        *http.Client
	issuer, api string
	now         func() time.Time
}

// New creates a client with fixed production endpoints and native credential storage.
func New(dir string) *Client {
	return &Client{dir: dir, store: nativeStore{}, http: &http.Client{Timeout: 180 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}, issuer: issuer, api: resource, now: time.Now}
}
func (c *Client) request(ctx context.Context, method, endpoint, contentType, bearer string, body io.Reader) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return nil, errors.New("invalid OAI endpoint")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	if strings.HasSuffix(endpoint, "/responses") {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if resp != nil {
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("OAI network request failed or redirect refused; check connection and retry")
	}
	return resp, nil
}
func (c *Client) jsonRequest(ctx context.Context, method, endpoint, contentType, bearer string, body io.Reader, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, e := c.request(ctx, method, endpoint, contentType, bearer, body)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	b, e := bounded(resp.Body, 1<<20)
	if e != nil {
		return e
	}
	if resp.StatusCode != http.StatusOK {
		return remoteError(resp.StatusCode, resp.Header.Get("x-request-id"), b)
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return errors.New("invalid OAI JSON response")
	}
	return nil
}
func (c *Client) discover(ctx context.Context) (discovery, error) {
	var d discovery
	e := c.jsonRequest(ctx, "GET", c.issuer+"/.well-known/openid-configuration", "", "", nil, &d)
	if e != nil {
		return d, e
	}
	if d.Issuer != c.issuer {
		return d, errors.New("OIDC discovery issuer mismatch")
	}
	for _, endpoint := range []string{d.Authorize, d.Token, d.JWKS, d.Revoke} {
		u, e := url.Parse(endpoint)
		base, _ := url.Parse(c.issuer)
		if e != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Path == "" {
			return d, errors.New("unsafe OIDC discovery endpoint")
		}
	}
	return d, nil
}

type apiError struct {
	Code      string
	Status    int
	RequestID string
}

func (e *apiError) Error() string {
	msg := "OAI request failed; check account permissions and connection"
	switch e.Code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		msg = "sign in again with rhizome auth oai"
	case "subscription_sharing_usage_limit_exceeded":
		msg = "ChatGPT usage limit reached; review https://chatgpt.com/settings/usage"
	case "subscription_sharing_v2_client_not_enabled":
		msg = "ChatGPT plan usage is not enabled for this client; check the integration's access"
	case "subscription_sharing_user_not_eligible":
		msg = "ChatGPT plan usage is unavailable for this account, workspace or policy"
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		msg = "ChatGPT usage temporarily unavailable; retry later"
	default:
		if e.Status == 429 {
			msg = "rate limited; retry later (request was not retried)"
		}
		if e.Status == 401 {
			msg = "authentication rejected; run rhizome auth oai"
		}
		if e.Status == 403 {
			msg = "permission or policy denied this request; check account and integration access"
		}
	}
	return fmt.Sprintf("%s (code=%s, HTTP=%d, request_id=%s)", msg, e.Code, e.Status, e.RequestID)
}
func terminalRefresh(err error) bool {
	var e *apiError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return false
}
func remoteError(status int, id string, b []byte) error {
	var raw struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	_ = json.Unmarshal(b, &raw)
	code := raw.Code
	if len(raw.Error) > 0 {
		var s string
		if json.Unmarshal(raw.Error, &s) == nil {
			code = s
		} else {
			var obj struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw.Error, &obj)
			code = obj.Code
		}
	}
	// Never echo arbitrary server text, which may include credentials or prompt data.
	known := map[string]bool{"invalid_grant": true, "invalid_refresh_token": true, "token_expired": true, "refresh_token_expired": true, "refresh_token_invalidated": true, "refresh_token_reused": true, "invalid_client": true, "subscription_sharing_usage_limit_exceeded": true, "subscription_sharing_v2_client_not_enabled": true, "subscription_sharing_user_not_eligible": true, "subscription_sharing_usage_unavailable": true, "subscription_sharing_user_unavailable": true, "subscription_sharing_unsupported_capability": true, "subscription_sharing_route_not_supported": true, "subscription_sharing_invalid_user": true, "chatpass_v2_scope_not_authorized": true, "chatpass_v2_invalid_authorization_context": true}
	if !known[code] {
		code = "unknown_error"
	}
	if len(id) > 100 || !safeLabel.MatchString(id) {
		id = "unavailable"
	}
	return &apiError{Code: code, Status: status, RequestID: id}
}
