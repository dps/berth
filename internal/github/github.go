// Package github covers the GitHub REST calls Berth makes (webhooks, branch
// heads, deploy keys, meta) plus device-flow login and webhook signature
// verification.
package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Token   string
	BaseURL string // https://api.github.com
	WebURL  string // https://github.com (device flow)
	HTTP    *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, BaseURL: "https://api.github.com", WebURL: "https://github.com", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// APIError carries the HTTP status so callers can fall back (e.g. to
// polling when the token cannot create hooks).
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

func IsStatus(err error, codes ...int) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		for _, c := range codes {
			if ae.Status == c {
				return true
			}
		}
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "berth")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return resp, &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil {
		if s, ok := out.(*string); ok {
			*s = strings.TrimSpace(string(data))
		} else if len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp, err
			}
		}
	}
	return resp, nil
}

func (c *Client) User(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	_, err := c.do(ctx, "GET", "/user", nil, &u, nil)
	return u.Login, err
}

// CreateHook registers a push webhook and returns its id.
func (c *Client) CreateHook(ctx context.Context, repo, hookURL, secret string) (int64, error) {
	body := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"push"},
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret, "insecure_ssl": "0"},
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_, err := c.do(ctx, "POST", "/repos/"+repo+"/hooks", body, &out, nil)
	return out.ID, err
}

func (c *Client) DeleteHook(ctx context.Context, repo string, id int64) error {
	_, err := c.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/hooks/%d", repo, id), nil, nil, nil)
	if IsStatus(err, 404) {
		return nil
	}
	return err
}

// BranchHead returns the head sha of branch. With a matching etag GitHub
// answers 304, which does not count against the rate limit.
func (c *Client) BranchHead(ctx context.Context, repo, branch, etag string) (sha, newETag string, notModified bool, err error) {
	hdr := map[string]string{"Accept": "application/vnd.github.sha"}
	if etag != "" {
		hdr["If-None-Match"] = etag
	}
	resp, err := c.do(ctx, "GET", "/repos/"+repo+"/commits/"+url.PathEscape(branch), nil, &sha, hdr)
	if err != nil {
		return "", "", false, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return "", etag, true, nil
	}
	return sha, resp.Header.Get("ETag"), false, nil
}

func (c *Client) AddDeployKey(ctx context.Context, repo, title, pubkey string) error {
	_, err := c.do(ctx, "POST", "/repos/"+repo+"/keys", map[string]any{"title": title, "key": pubkey, "read_only": true}, nil, nil)
	return err
}

// HookRanges returns GitHub's webhook source IP ranges from /meta.
func (c *Client) HookRanges(ctx context.Context) ([]string, error) {
	var meta struct {
		Hooks []string `json:"hooks"`
	}
	_, err := c.do(ctx, "GET", "/meta", nil, &meta, nil)
	return meta.Hooks, err
}

// ---- device flow ----

// Device flow needs a GitHub OAuth app with device flow enabled. Berth has
// no published app of its own yet, so the client id comes from
// BERTH_GITHUB_CLIENT_ID; without it `berth init` uses `gh auth token` or
// asks for a token (--github-token).

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (c *Client) form(ctx context.Context, path string, v url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.WebURL+path, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) StartDeviceFlow(ctx context.Context, clientID string, scopes []string) (*DeviceCode, error) {
	dc := &DeviceCode{}
	err := c.form(ctx, "/login/device/code", url.Values{"client_id": {clientID}, "scope": {strings.Join(scopes, " ")}}, dc)
	if err == nil && dc.DeviceCode == "" {
		err = fmt.Errorf("github device flow: empty response (is client id %q a device-flow-enabled OAuth app?)", clientID)
	}
	return dc, err
}

// PollDeviceFlow waits for the user to approve and returns the token.
func (c *Client) PollDeviceFlow(ctx context.Context, clientID string, dc *DeviceCode) (string, error) {
	interval := time.Duration(dc.Interval) * time.Second
	if interval == 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		var out struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		err := c.form(ctx, "/login/oauth/access_token", url.Values{
			"client_id":   {clientID},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &out)
		if err != nil {
			return "", err
		}
		switch out.Error {
		case "":
			return out.AccessToken, nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return "", fmt.Errorf("github device flow: %s", out.Error)
		}
	}
	return "", fmt.Errorf("github device flow: code expired")
}

// ---- webhooks ----

// VerifySignature checks X-Hub-Signature-256 ("sha256=<hex>") in constant time.
func VerifySignature(secret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type PushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Before  string `json:"before"`
	Deleted bool   `json:"deleted"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
	HeadCommit *struct {
		Message string `json:"message"`
	} `json:"head_commit"`
}

// Touches reports whether the push changed anything under path. GitHub
// truncates commit lists at 20, so long pushes are assumed to touch it.
func (p *PushEvent) Touches(path string) bool {
	path = strings.Trim(path, "/")
	if path == "" || len(p.Commits) == 0 || len(p.Commits) >= 20 {
		return true
	}
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range list {
				if f == path || strings.HasPrefix(f, path+"/") {
					return true
				}
			}
		}
	}
	return false
}
