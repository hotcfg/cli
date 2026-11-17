package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxResponseBytes bounds a response body (bins are at most 1 MiB).
const maxResponseBytes = 32 << 20

type client struct {
	http *http.Client
	t    target
	ua   string
}

func (a *App) client(t target) *client {
	hc := a.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	if t.Key != "" && strings.HasPrefix(t.APIURL, "http://") && !isLoopback(t.APIURL) {
		fmt.Fprintf(a.Stderr, "warning: sending your API key over unencrypted HTTP to %s\n", t.APIURL)
	}
	return &client{http: hc, t: t, ua: a.userAgent()}
}

func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// apiError is a non-2xx API response.
type apiError struct {
	Status  int
	Code    string
	Message string
	Details json.RawMessage
}

func (e *apiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	s := fmt.Sprintf("%s (HTTP %d", msg, e.Status)
	if e.Code != "" {
		s += ", " + e.Code
	}
	s += ")"
	switch {
	case e.Code == "use_private_api_host":
		s += "\nhint: publishable keys read private bins on the API's private host; for an API other than " + DefaultAPIURL + ", set HOTCFG_PRIVATE_API_URL to it"
	case e.Code == "key_expired":
		s += "\nhint: create a new key in the dashboard, then run 'hotcfg login'"
	case e.Code == "private_bins_require_pro":
		s += "\nhint: on the Free plan bins are public: add --public (anyone with the bin's URL can read it), or upgrade the organization to Pro"
	case e.Code == "key_scope_denied" && strings.Contains(e.Message, "production"):
		s += "\nhint: this key is limited to other environments, so it can't change production; an administrator can give you a key that includes production"
	case e.Code == "key_creator_not_admin":
		s += "\nhint: whoever created this key is no longer an administrator; ask an administrator for a new key"
	case e.Code == "production_admin_only":
		s += "\nhint: only administrators change production; push to staging, and ask an administrator to promote it"
	case e.Code == "key_scope_denied":
		s += "\nhint: this key is limited to some bins or environments; 'hotcfg login' shows which"
	case e.Status == http.StatusUnauthorized:
		s += "\nhint: check your API key; run 'hotcfg login' to save a new one"
	case e.Code == "forbidden":
		s += "\nhint: this command needs a secret key (sk_...); publishable keys are read-only"
	}
	return s
}

// do sends a request to path (relative to /v1) and returns the body of a
// 2xx response, or an *apiError.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, http.Header, error) {
	if method == http.MethodGet && isPublishableKey(c.t.Key) && isBareBinPath(path) {
		// /v1/bins/<id> means "in the key's organization", which only the
		// main API can resolve, and it refuses publishable-key reads of
		// private bins (421). The private host needs the organization.
		return nil, nil, errors.New("with a publishable key, name the bin as org/bin (e.g. acme/flags)")
	}
	// Bin reads with a publishable key go straight to the private host
	// (DefaultPrivateAPIURL, or $HOTCFG_PRIVATE_API_URL). If it can't be
	// reached (not set up yet, or down), the read goes to the API below.
	// The CLI never sends a key to a host the API names.
	if method == http.MethodGet && c.t.PrivateURL != "" && isPublishableKey(c.t.Key) && isOrgBinPath(path) {
		u := c.t.PrivateURL + "/v1" + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		b, h, err := c.doURL(ctx, method, u, nil)
		var ae *apiError
		if err == nil || errors.As(err, &ae) {
			return b, h, err
		}
	}
	return c.doURL(ctx, method, c.url(path, query), body)
}

// isOrgBinPath reports whether path (relative to /v1) reads a bin by
// organization: /<org>/<bin>, the only form the private host serves.
func isOrgBinPath(path string) bool {
	org, bin, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || org == "" || bin == "" || strings.Contains(bin, "/") {
		return false
	}
	switch org {
	case "auth", "bins", "organizations", "invites", "webhooks", "plans", "status":
		return false
	}
	return true
}

// isBareBinPath reports whether path (relative to /v1) reads a bin by id
// alone: /bins/<id>.
func isBareBinPath(path string) bool {
	id, ok := strings.CutPrefix(path, "/bins/")
	return ok && id != "" && !strings.Contains(id, "/")
}

func (c *client) url(path string, query url.Values) string {
	u := c.t.APIURL + "/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// doURL is do for a full URL.
func (c *client) doURL(ctx context.Context, method, u string, body []byte) ([]byte, http.Header, error) {
	resp, err := c.sendURL(ctx, method, u, body)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read response from %s: %w", c.t.APIURL, err)
	}
	if len(b) > maxResponseBytes {
		return nil, nil, fmt.Errorf("response from %s is larger than %d bytes", c.t.APIURL, maxResponseBytes)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return b, resp.Header, nil
	}
	return nil, resp.Header, parseAPIError(resp.StatusCode, b)
}

// stream sends a GET and returns the open body of a 2xx response, which
// the caller closes, for responses too large to hold in memory (export).
func (c *client) stream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	resp, err := c.send(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.Body, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, parseAPIError(resp.StatusCode, b)
}

// send builds and sends one API request.
func (c *client) send(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Response, error) {
	return c.sendURL(ctx, method, c.url(path, query), body)
}

// sendURL sends one request to a full URL.
func (c *client) sendURL(ctx context.Context, method, u string, body []byte) (*http.Response, error) {
	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rb)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.t.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.t.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("cannot reach %s: %w", c.t.APIURL, err)
	}
	return resp, nil
}

// parseAPIError turns a non-2xx response body into an *apiError.
func parseAPIError(status int, b []byte) *apiError {
	ae := &apiError{Status: status}
	var eb struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &eb) == nil && eb.Error.Code != "" {
		ae.Code, ae.Message, ae.Details = eb.Error.Code, eb.Error.Message, eb.Error.Details
	} else if s := strings.TrimSpace(string(b)); s != "" && len(s) < 300 {
		ae.Message = s // not our API (proxy error page, wrong --api-url, ...)
	}
	return ae
}

// doJSON is do with a JSON request body and a decoded JSON response.
func (c *client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	b, _, err := c.do(ctx, method, path, nil, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("unexpected response from %s: %w", c.t.APIURL, err)
	}
	return nil
}

// writeResult is the API's response to PUT, rollback and promote.
type writeResult struct {
	BinID               string `json:"bin_id"`
	Environment         string `json:"environment"`
	Version             int    `json:"version"`
	Hash                string `json:"hash"`
	RolledBackTo        int    `json:"rolled_back_to"`
	PromotedFrom        string `json:"promoted_from"`
	PromotedFromVersion int    `json:"promoted_from_version"`
	// Rollout is set when a promote started a progressive rollout: Version
	// is then the candidate.
	Rollout *rollout `json:"rollout"`
}

// rollout is a running progressive rollout.
type rollout struct {
	Version        int `json:"version"`
	Percent        int `json:"percent"`
	CurrentVersion int `json:"current_version"`
}

// binPath is the API path of a bin: "acme/flags" names it by organization
// slug (/acme/flags); a bare id is looked up in the key's own organization
// (/bins/flags).
func binPath(id string, suffix string) string {
	if org, bin, ok := strings.Cut(id, "/"); ok {
		return "/" + url.PathEscape(org) + "/" + url.PathEscape(bin) + suffix
	}
	return "/bins/" + url.PathEscape(id) + suffix
}
