package homelab

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// server answers like the daemon's stack listener: it wants the bearer
// token and records every request line.
func server(t *testing.T, h http.HandlerFunc) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,"code":"unauthorized"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/api/", " tok\n", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestServicesToleratesExtraFields(t *testing.T) {
	t.Parallel()
	c, seen := server(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"media-plex","type":"Native","stack":"media","active_state":"active",
			"sub_state":"running","description":"Plex","unit_name":"plex.service","update_available":false,
			"started_at":"2026-11-16T08:00:00Z","usage":{"cpu":1.5},"something_new":{"x":1}},
			{"name":"caddy","active_state":"active","sub_state":"running"}]`))
	})
	got, err := c.Services(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Service{Name: "media-plex", Stack: "media", ActiveState: "active", SubState: "running"}) || got[1].Stack != "" {
		t.Fatalf("services = %+v", got)
	}
	if seen()[0] != "GET /api/services" {
		t.Fatalf("request = %v", seen())
	}
}

func TestBackupsAndVersion(t *testing.T) {
	t.Parallel()
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/backups":
			_, _ = w.Write([]byte(`[{"id":7,"name":"App data","enabled":true,"schedule":"0 4 * * *","sources":[],
				"destinations":[],"last_run":{"id":41,"backup_id":7,"backup_name":"App data","trigger":"cron",
				"started_at":"2026-11-16T04:00:00Z","finished_at":"2026-11-16T04:10:00Z","result":"partial",
				"error":"media: Database dumps to B2 failed","files":12,"bytes":3400},
				"last_success":null,"next_run":null,"overdue":true,"running":false},
				{"id":8,"name":"Photos","last_run":null,"next_run":null,"overdue":false}]`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.3.0","commit":"abc1234","extra":true}`))
		}
	})
	bks, err := c.Backups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 16, 4, 10, 0, 0, time.UTC)
	if len(bks) != 2 || bks[0].ID != 7 || bks[0].Name != "App data" || !bks[0].Overdue || bks[0].NextRun != nil ||
		bks[0].LastRun == nil || bks[0].LastRun.Result != "partial" ||
		bks[0].LastRun.FinishedAt == nil || !bks[0].LastRun.FinishedAt.Equal(want) || bks[1].LastRun != nil {
		t.Fatalf("backups = %+v", bks)
	}
	v, err := c.Version(t.Context())
	if err != nil || v != (Version{Version: "0.3.0", Commit: "abc1234"}) {
		t.Fatalf("version = %+v, %v", v, err)
	}
}

func TestActionsPostAndEscapeTheUnit(t *testing.T) {
	t.Parallel()
	c, seen := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, f := range []func() error{
		func() error { return c.Start(t.Context(), "media-plex") },
		func() error { return c.Stop(t.Context(), "media-plex") },
		func() error { return c.Restart(t.Context(), "a/../b") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	want := "POST /api/services/media-plex/start,POST /api/services/media-plex/stop,POST /api/services/a%2F..%2Fb/restart"
	if got := strings.Join(seen(), ","); got != want {
		t.Fatalf("requests = %s", got)
	}
}

func TestProblemErrors(t *testing.T) {
	t.Parallel()
	c, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/services/caddy/stop":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Forbidden","status":403,"detail":"caddy is a core unit","code":"core_unit"}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		}
	})
	err := c.Stop(t.Context(), "caddy")
	var p *Problem
	if !errors.As(err, &p) || p.Status != 403 || p.Code != "core_unit" || err.Error() != "caddy is a core unit (core_unit)" {
		t.Fatalf("err = %v (%+v)", err, p)
	}
	_, err = c.Services(t.Context())
	if !errors.As(err, &p) || p.Status != 502 || err.Error() != "Bad Gateway" {
		t.Fatalf("non-JSON error = %v", err)
	}
}

func TestWrongTokenIsUnauthorized(t *testing.T) {
	t.Parallel()
	c, _ := server(t, func(http.ResponseWriter, *http.Request) {})
	c.token = "other"
	_, err := c.Services(t.Context())
	var p *Problem
	if !errors.As(err, &p) || p.Status != 401 || p.Code != "unauthorized" {
		t.Fatalf("err = %v", err)
	}
}

func TestUnconfigured(t *testing.T) {
	t.Parallel()
	for _, in := range [][2]string{{"", "tok"}, {"http://10.99.3.1:7781/api", ""}, {"", ""}} {
		c, err := New(in[0], in[1], nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.Configured() {
			t.Fatalf("%q configured", in)
		}
		if _, err := c.Services(t.Context()); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("%q: err = %v", in, err)
		}
	}
	for _, bad := range []string{"10.99.3.1:7781/api", "ftp://x/api", "http://"} {
		if _, err := New(bad, "tok", nil); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
}
