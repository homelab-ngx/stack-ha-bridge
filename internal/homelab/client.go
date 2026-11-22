// Package homelab is a hand-written client for the few homelab daemon API
// calls the bridge makes, over the stack listener with a bearer token. The
// paths and shapes follow the daemon's published OpenAPI document; fields
// the bridge does not use are ignored, so new ones never break it.
package homelab

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

// Service is one row of GET /services. Stack is empty for a core unit on
// the host, which can never be controlled from a stack.
type Service struct {
	Name        string `json:"name"`
	Stack       string `json:"stack,omitempty"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// Backup is one row of GET /backups. ID is stable across a rename; Name is
// what the operator called it.
type Backup struct {
	ID      int64      `json:"id"`
	Name    string     `json:"name"`
	LastRun *BackupRun `json:"last_run"` // nil: it never ran
	NextRun *time.Time `json:"next_run"`
	Overdue bool       `json:"overdue"`
}

// BackupRun is a backup's newest run. FinishedAt is nil while it runs.
type BackupRun struct {
	Result     string     `json:"result"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Version is GET /version.
type Version struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Problem is the daemon's RFC 9457 error body. Code is the machine-readable
// reason a client can branch on.
type Problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

func (p *Problem) Error() string {
	msg := p.Detail
	if msg == "" {
		msg = p.Title
	}
	if msg == "" {
		msg = http.StatusText(p.Status)
	}
	if p.Code != "" {
		return msg + " (" + p.Code + ")"
	}
	return msg
}

// ErrNotConfigured is returned by every call when the stack has no API URL
// or token, i.e. the api grant is not approved.
var ErrNotConfigured = errors.New("homelab API not configured (no HOMELAB_API_URL/HOMELAB_API_TOKEN)")

// Client calls the daemon's stack API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for base (e.g. http://10.99.3.1:7781/api). An empty
// base or token gives a client whose calls fail with ErrNotConfigured.
func New(base, token string, hc *http.Client) (*Client, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base != "" {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("HOMELAB_API_URL %q is not an http(s) URL", base)
		}
	}
	return &Client{base: base, token: strings.TrimSpace(token), http: hc}, nil
}

// Configured reports whether the client has a URL and a token.
func (c *Client) Configured() bool { return c.base != "" && c.token != "" }

// Services lists the service units (GET /services, scope read).
func (c *Client) Services(ctx context.Context) ([]Service, error) {
	var out []Service
	return out, c.do(ctx, http.MethodGet, "/services", &out)
}

// Backups lists the backups (GET /backups, scope read).
func (c *Client) Backups(ctx context.Context) ([]Backup, error) {
	var out []Backup
	return out, c.do(ctx, http.MethodGet, "/backups", &out)
}

// Version returns the daemon's build (GET /version, scope read).
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	return out, c.do(ctx, http.MethodGet, "/version", &out)
}

// Start, Stop and Restart control one stack unit
// (POST /services/{unit}/start|stop|restart, scope control).
func (c *Client) Start(ctx context.Context, unit string) error { return c.action(ctx, unit, "start") }
func (c *Client) Stop(ctx context.Context, unit string) error  { return c.action(ctx, unit, "stop") }
func (c *Client) Restart(ctx context.Context, unit string) error {
	return c.action(ctx, unit, "restart")
}

func (c *Client) action(ctx context.Context, unit, verb string) error {
	return c.do(ctx, http.MethodPost, "/services/"+url.PathEscape(unit)+"/"+verb, nil)
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json, application/problem+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		p := &Problem{Status: resp.StatusCode}
		if json.Unmarshal(body, p) != nil || p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return p
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}
