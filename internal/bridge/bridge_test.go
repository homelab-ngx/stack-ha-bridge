package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

// The bridge tests run in synctest bubbles: time is virtual, so the 30 s
// ticker, the readback wait and the API backoff cost nothing.

type published struct {
	topic   string
	payload string
	retain  bool
}

type fakePub struct {
	mu   sync.Mutex
	msgs []published
}

func (p *fakePub) publish(_ context.Context, topic string, payload []byte, retain bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, published{topic, string(payload), retain})
	return nil
}

func (p *fakePub) take() []published {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.msgs
	p.msgs = nil
	return out
}

func (p *fakePub) last(topic string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.msgs) - 1; i >= 0; i-- {
		if p.msgs[i].topic == topic {
			return p.msgs[i].payload, true
		}
	}
	return "", false
}

func (p *fakePub) count(topic string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.msgs {
		if m.topic == topic {
			n++
		}
	}
	return n
}

// fakeAPI stands in for the daemon's stack API.
type fakeAPI struct {
	mu       sync.Mutex
	services []homelab.Service
	backups  []homelab.Backup
	down     bool // reads fail as if the daemon did not answer
	reads    int
	calls    []string
	err      error
	block    map[string]chan struct{} // name → released when closed
}

var errDown = errors.New("dial tcp 10.99.7.1:7781: connect: connection refused")

func (f *fakeAPI) Services(context.Context) ([]homelab.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.down {
		return nil, errDown
	}
	return f.services, nil
}

func (f *fakeAPI) Backups(context.Context) ([]homelab.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errDown
	}
	return f.backups, nil
}

func (f *fakeAPI) Version(context.Context) (homelab.Version, error) {
	return homelab.Version{Version: "0.3.0", Commit: "abc1234"}, nil
}

func (f *fakeAPI) do(verb, name string) error {
	f.mu.Lock()
	f.calls = append(f.calls, verb+" "+name)
	ch, err := f.block[name], f.err
	f.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return err
}

func (f *fakeAPI) Start(_ context.Context, n string) error   { return f.do("start", n) }
func (f *fakeAPI) Stop(_ context.Context, n string) error    { return f.do("stop", n) }
func (f *fakeAPI) Restart(_ context.Context, n string) error { return f.do("restart", n) }

func (f *fakeAPI) set(fn func(*fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAPI) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		services: []homelab.Service{
			{Name: "media-foo", Stack: "media", ActiveState: "active"},
			{Name: "calibre-web", Stack: "media", ActiveState: "active"},
			{Name: "caddy"},
		},
		backups: []homelab.Backup{{ID: 7, Name: "App data", LastRun: &homelab.BackupRun{Result: "success"}}},
	}
}

type harness struct {
	b   *Bridge
	pub *fakePub
	api *fakeAPI
}

// start runs a connected bridge loop and waits out the discovery readback.
// retained, if set, is the discovery payload the broker hands back.
func start(t *testing.T, api *fakeAPI, retained string) *harness {
	t.Helper()
	h := &harness{pub: &fakePub{}, api: api}
	h.b = newBridge(Config{DiscoveryPrefix: "homeassistant", Rev: "f00ba12"}, api, newHostReader(fixture(t, hostFiles())))
	h.b.pub = h.pub
	h.b.connected.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	h.b.runCtx = ctx
	go h.b.loop(ctx)
	h.b.send(event{kind: evConnected})
	if retained != "" {
		h.b.handle("homeassistant/device/homelab/config", []byte(retained), true)
	}
	time.Sleep(readbackWait + time.Millisecond)
	synctest.Wait()
	return h
}

// command delivers a non-retained command and waits for its outcome.
func (h *harness) command(topic, payload string) {
	h.b.handle(topic, []byte(payload), false)
	synctest.Wait()
}

func topics(msgs []published) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.topic)
	}
	return out
}

const discoveryTopic = "homeassistant/device/homelab/config"

func TestConnectRemovesVanishedComponentsThenGoesOnline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// What the daemon retained before the bridge moved into a stack.
		h := start(t, newFakeAPI(), `{"components":{
			"homelab_media_foo":{"platform":"switch"},"homelab_network_in":{"platform":"sensor"},
			"homelab_disk_usage":{"platform":"sensor"},"homelab_pc":{"platform":"binary_sensor"},
			"homelab_pc_wake":{"platform":"button"},"homelab_backup_b2_backup_run":{"platform":"button"}}}`)
		msgs := h.pub.take()
		if len(msgs) < 4 {
			t.Fatalf("published %v", topics(msgs))
		}
		first, second := decode(t, []byte(msgs[0].payload)), decode(t, []byte(msgs[1].payload))
		if msgs[0].topic != discoveryTopic || !msgs[0].retain {
			t.Fatalf("first publish = %+v, want retained discovery", msgs[0])
		}
		for _, k := range []string{"homelab_network_in", "homelab_disk_usage", "homelab_pc", "homelab_pc_wake", "homelab_backup_b2_backup_run"} {
			if c := first.Components[k]; len(c) != 1 || c["platform"] == nil {
				t.Errorf("first discovery %s = %v, want platform-only", k, c)
			}
			if _, ok := second.Components[k]; ok {
				t.Errorf("second discovery still lists %s", k)
			}
		}
		if second.Components["homelab_media_foo"] == nil || second.Device["sw_version"] != "0.3.0 (abc1234)" {
			t.Fatalf("second discovery = %v", second)
		}
		if msgs[2] != (published{"homelab/status", "online", true}) {
			t.Fatalf("third publish = %+v, want retained online", msgs[2])
		}
		if msgs[3] != (published{"homelab/api/status", "online", true}) {
			t.Fatalf("fourth publish = %+v, want retained API online", msgs[3])
		}
		for _, m := range msgs[4:] {
			if m.retain {
				t.Fatalf("state %s published retained", m.topic)
			}
		}
		var host map[string]any
		_ = json.Unmarshal([]byte(msgs[4].payload), &host)
		if msgs[4].topic != "homelab/host/state" || host["mem_usage"] != 75.0 || host["cpu_temp"] != 35.0 {
			t.Fatalf("host state = %+v", msgs[4])
		}
	})
}

func TestServiceCommandsResolveSlugsToNames(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.pub.take()
		h.command("homelab/svc/media_foo/set", "OFF")
		h.command("homelab/svc/calibre_web/set", "ON")
		h.command("homelab/svc/calibre_web/restart", "PRESS")

		want := []string{"stop media-foo", "start calibre-web", "restart calibre-web"}
		if got := h.api.called(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("API calls = %v, want %v", got, want)
		}
		if got, _ := h.pub.last("homelab/last_command/state"); got != "restart calibre-web: ok" {
			t.Fatalf("last command = %q", got)
		}
		// Each command rereads and republishes state right away rather than at
		// the next tick.
		if got, ok := h.pub.last("homelab/svc/media_foo/state"); !ok || got != "ON" {
			t.Fatalf("media-foo state after the command = %q (published %v)", got, ok)
		}
	})
}

func TestIgnoredCommands(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.b.handle("homelab/svc/media_foo/set", []byte("OFF"), true) // retained: would replay on reconnect
		h.command("homelab/svc/nope/restart", "PRESS")               // not a stack unit
		h.command("homelab/svc/caddy/set", "OFF")                    // core unit, no entity
		h.command("homelab/svc/media_foo/set", "MAYBE")
		h.command("homelab/backup/b2_backup/run", "PRESS") // backups are read-only now
		h.command("homelab/pc/wake", "PRESS")              // the PC entities are gone
		synctest.Wait()
		if got := h.api.called(); len(got) != 0 {
			t.Fatalf("API calls = %v, want none", got)
		}
	})
}

func TestCommandErrorsShowInLastCommand(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.api.set(func(f *fakeAPI) {
			f.err = &homelab.Problem{Status: 403, Detail: "token lacks the control scope", Code: "forbidden"}
		})
		h.command("homelab/svc/media_foo/set", "OFF")
		if got, _ := h.pub.last("homelab/last_command/state"); got != "stop media-foo: token lacks the control scope (forbidden)" {
			t.Fatalf("last command = %q", got)
		}
		// The loop is still serving.
		h.api.set(func(f *fakeAPI) { f.err = nil })
		h.command("homelab/svc/media_foo/set", "ON")
		if got, _ := h.pub.last("homelab/last_command/state"); got != "start media-foo: ok" {
			t.Fatalf("last command after an error = %q", got)
		}
	})
}

func TestCommandsSerializePerTarget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		release := make(chan struct{})
		h.api.set(func(f *fakeAPI) { f.block = map[string]chan struct{}{"media-foo": release} })

		h.command("homelab/svc/media_foo/set", "OFF")       // blocks
		h.command("homelab/svc/media_foo/restart", "PRESS") // waits behind it
		h.command("homelab/svc/calibre_web/set", "OFF")     // other target: runs now
		if got := h.api.called(); strings.Join(got, ",") != "stop media-foo,stop calibre-web" {
			t.Fatalf("calls while media-foo is blocked = %v", got)
		}
		close(release)
		synctest.Wait()
		if got := h.api.called(); len(got) != 3 || got[2] != "restart media-foo" {
			t.Fatalf("calls after release = %v", got)
		}
	})
}

func TestBirthsCollapseIntoOneRepublish(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.pub.take()
		for range 3 {
			h.b.handle("homeassistant/status", []byte("online"), false)
		}
		time.Sleep(birthJitter)
		synctest.Wait()
		if n := h.pub.count(discoveryTopic); n != 1 {
			t.Fatalf("discovery published %d times after three births, want 1", n)
		}
	})
}

func TestStateTicksEveryThirtySeconds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.pub.take()
		time.Sleep(stateInterval)
		synctest.Wait()
		got := strings.Join(topics(h.pub.take()), ",")
		for _, want := range []string{"homelab/host/state", "homelab/svc/media_foo/state", "homelab/backup/7/state", "homelab/api/status"} {
			if !strings.Contains(got, want) {
				t.Fatalf("tick published %v, missing %s", got, want)
			}
		}
		if strings.Contains(got, discoveryTopic) {
			t.Fatal("an unchanged tick republished discovery")
		}
	})
}

func TestNewStackServiceRepublishesDiscovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.pub.take()
		h.api.set(func(f *fakeAPI) {
			f.services = append(f.services, homelab.Service{Name: "ha-homelab-ha", Stack: "ha", ActiveState: "active"})
		})
		time.Sleep(stateInterval)
		synctest.Wait()
		payload, ok := h.pub.last(discoveryTopic)
		if !ok || decode(t, []byte(payload)).Components["homelab_ha_homelab_ha"] == nil {
			t.Fatal("a new stack service did not reach discovery")
		}
		if got, _ := h.pub.last("homelab/svc/ha_homelab_ha/state"); got != "ON" {
			t.Fatalf("new service state = %q", got)
		}
	})
}

func TestAPIDownAtStartLeavesRetainedDiscoveryAlone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		api := newFakeAPI()
		api.down = true
		h := start(t, api, `{"components":{"homelab_media_foo":{"platform":"switch"}}}`)
		msgs := h.pub.take()
		if n := strings.Count(strings.Join(topics(msgs), ","), discoveryTopic); n != 0 {
			t.Fatalf("discovery published %d times without the API: it would delete every service", n)
		}
		if got, _ := lastIn(msgs, "homelab/status"); got != "online" {
			t.Fatalf("bridge status = %q, want online: MQTT stays up", got)
		}
		if got, _ := lastIn(msgs, "homelab/api/status"); got != "offline" {
			t.Fatalf("API status = %q, want offline", got)
		}
		if _, ok := lastIn(msgs, "homelab/host/state"); !ok {
			t.Fatal("host sensors not published while the API is down")
		}

		// The daemon comes back; the retry (5 s after the first failure)
		// publishes discovery without waiting for the next tick.
		api.set(func(f *fakeAPI) { f.down = false })
		time.Sleep(apiRetryFirst)
		synctest.Wait()
		payload, ok := h.pub.last(discoveryTopic)
		if !ok || decode(t, []byte(payload)).Components["homelab_media_foo"] == nil {
			t.Fatal("discovery not published once the API answered")
		}
		if got, _ := h.pub.last("homelab/api/status"); got != "online" {
			t.Fatalf("API status = %q after recovery", got)
		}
	})
}

func TestAPIOutageBacksOffAndKeepsLastState(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := start(t, newFakeAPI(), "")
		h.pub.take()
		h.api.set(func(f *fakeAPI) { f.down = true })
		before := h.api.readCount()

		time.Sleep(stateInterval) // tick: the read fails
		synctest.Wait()
		if got, _ := h.pub.last("homelab/api/status"); got != "offline" {
			t.Fatalf("API status = %q, want offline", got)
		}
		if got, _ := h.pub.last("homelab/svc/media_foo/state"); got != "ON" {
			t.Fatalf("last known service state = %q, want ON kept", got)
		}
		if h.pub.count(discoveryTopic) != 0 {
			t.Fatal("an API outage republished discovery")
		}

		// Ten minutes down: reads back off (5, 10, 20, 40, 80, 120, 120 … s)
		// instead of following the 30 s tick, and the bridge keeps going.
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if n := h.api.readCount() - before; n < 5 || n > 12 {
			t.Fatalf("%d reads in ten minutes down", n)
		}
		if got, _ := h.pub.last("homelab/host/state"); got == "" {
			t.Fatal("host state stopped during the outage")
		}
	})
}

func lastIn(msgs []published, topic string) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].topic == topic {
			return msgs[i].payload, true
		}
	}
	return "", false
}
