// Package bridge publishes the homelab to Home Assistant over MQTT
// discovery and turns the switch and button presses HA sends back into
// calls on the homelab daemon's stack API.
package bridge

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

var log = slog.Default()

const (
	stateInterval = 30 * time.Second
	// readbackWait is how long a fresh connection waits for the broker to
	// hand back the retained discovery payload before publishing.
	readbackWait = 2 * time.Second
	// birthJitter spreads republishing after an HA restart, as HA asks.
	birthJitter    = 5 * time.Second
	commandTimeout = 2 * time.Minute
	// apiRetryFirst and apiRetryMax bound the backoff while the daemon API
	// does not answer; the state ticker does not read it meanwhile.
	apiRetryFirst = 5 * time.Second
	apiRetryMax   = 2 * time.Minute
)

// Config is how the bridge reaches the broker and labels the HA device.
type Config struct {
	BrokerURL       string
	Username        string
	PasswordFile    string
	ClientID        string
	DiscoveryPrefix string
	DashboardURL    string
	// Rev is the bridge's own build, shown as the discovery origin.
	Rev string
	// HostRoot is where /proc and /sys are read from; "/" in production.
	HostRoot string
}

// API is the part of the daemon's stack API the bridge uses.
type API interface {
	Services(ctx context.Context) ([]homelab.Service, error)
	Backups(ctx context.Context) ([]homelab.Backup, error)
	Version(ctx context.Context) (homelab.Version, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Restart(ctx context.Context, name string) error
}

// publisher is the part of the MQTT client the bridge publishes through.
type publisher interface {
	publish(ctx context.Context, topic string, payload []byte, retain bool) error
}

type eventKind int

const (
	evConnected eventKind = iota
	evBirth
	evDiscovery // the broker handed back the retained discovery payload
	evResult    // a command finished
)

type event struct {
	kind    eventKind
	payload []byte
	text    string
}

// Bridge is the Home Assistant integration.
type Bridge struct {
	cfg  Config
	api  API
	host *hostReader
	caps hostCaps

	pub       publisher
	events    chan event
	connected atomic.Bool

	mu      sync.Mutex
	idx     index
	targets map[string]chan struct{} // one-slot semaphores: one command per target at a time

	// runCtx bounds command execution. Set once before any MQTT callback
	// can run, read-only after.
	runCtx context.Context

	// Owned by the loop goroutine.
	prev       []byte // the discovery payload currently retained on the broker
	lastCmd    string
	discovered bool // discovery was published on this connection for the current index
	services   []homelab.Service
	backups    []homelab.Backup
	version    homelab.Version
	apiOK      bool // the last read of the daemon API succeeded
	apiSeen    bool // a read has succeeded since the bridge started
	apiFails   int
	retry      <-chan time.Time // set while backing off after a failed read
}

// New builds the bridge.
func New(cfg Config, api API) *Bridge {
	root := cfg.HostRoot
	if root == "" {
		root = "/"
	}
	return newBridge(cfg, api, newHostReader(root))
}

func newBridge(cfg Config, api API, host *hostReader) *Bridge {
	return &Bridge{
		cfg: cfg, api: api, host: host,
		caps: hostCaps{
			CPU: host.hasCPU, Mem: host.hasMem,
			CPUTemp: host.cpuTemp != "", NVMeTemp: host.nvmeTemp != "",
			Boot: !host.boot.IsZero(),
		},
		events:  make(chan event, 64),
		targets: map[string]chan struct{}{},
		lastCmd: "none",
	}
}

func (b *Bridge) currentIndex() index {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.idx
}

func (b *Bridge) discoveryTopic() string {
	return b.cfg.DiscoveryPrefix + "/device/homelab/config"
}

func (b *Bridge) birthTopic() string { return b.cfg.DiscoveryPrefix + "/status" }

// readPassword returns the broker password, or "" when the file is missing
// or empty. It is read at every connect, so a rotation lands on the next one.
func readPassword(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (b *Bridge) send(ev event) {
	select {
	case b.events <- ev:
	default:
		log.Warn("event queue full, dropping", "kind", ev.kind)
	}
}

// loop is the single publisher: every trigger only signals it, so state
// never lands before discovery and a stale snapshot never overwrites a
// fresh one.
func (b *Bridge) loop(ctx context.Context) {
	tick := time.NewTicker(stateInterval)
	defer tick.Stop()
	b.host.sample() // prime the CPU counters
	b.refresh(ctx, false)
	var readback, birth <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-b.events:
			switch ev.kind {
			case evConnected:
				b.discovered = false
				readback = time.After(readbackWait)
			case evDiscovery:
				b.prev = ev.payload
			case evBirth:
				if birth == nil {
					birth = time.After(time.Duration(rand.Int64N(int64(birthJitter))))
				}
			case evResult:
				b.lastCmd = ev.text
				b.refresh(ctx, true)
				b.publish(ctx)
			}
		case <-readback:
			readback = nil
			b.publishAll(ctx)
		case <-birth:
			birth = nil
			b.publishAll(ctx)
		case <-b.retry:
			b.retry = nil
			b.refresh(ctx, false)
			b.publish(ctx)
		case <-tick.C:
			b.refresh(ctx, false)
			b.publish(ctx)
		}
	}
}

// refresh reads the services, backups and version from the daemon. While a
// retry is pending only force reads (after a command). On failure the last
// good values are kept and the API entities go unavailable.
func (b *Bridge) refresh(ctx context.Context, force bool) {
	if b.retry != nil && !force {
		return
	}
	svcs, err := b.api.Services(ctx)
	var bks []homelab.Backup
	if err == nil {
		bks, err = b.api.Backups(ctx)
	}
	if err == nil && (!b.apiOK || b.version == (homelab.Version{})) {
		var v homelab.Version
		if v, err = b.api.Version(ctx); err == nil && v != b.version {
			b.version = v
			b.discovered = false // sw_version changed
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		b.apiOK = false
		b.apiFails++
		if b.apiFails == 1 || b.apiFails%10 == 0 {
			log.Warn("homelab API unreachable; keeping MQTT up", "failures", b.apiFails, "error", err)
		}
		if b.retry == nil {
			wait := min(apiRetryFirst<<min(b.apiFails-1, 8), apiRetryMax)
			b.retry = time.After(wait)
		}
		return
	}
	if b.apiFails > 0 {
		log.Info("homelab API answering again", "after_failures", b.apiFails)
	}
	b.apiOK, b.apiSeen, b.apiFails, b.retry = true, true, 0, nil
	b.services, b.backups = svcs, bks
	if idx, err := newIndex(svcs, bks); err == nil && !sameIndex(idx, b.currentIndex()) {
		b.discovered = false // a stack or backup came or went
	}
}

func sameIndex(a, b index) bool {
	return a.services != nil && b.services != nil &&
		maps.Equal(a.services, b.services) && maps.Equal(a.backups, b.backups)
}

// publish sends discovery when it is due and every state otherwise.
func (b *Bridge) publish(ctx context.Context) {
	if b.apiSeen && !b.discovered {
		b.publishAll(ctx)
		return
	}
	b.publishState(ctx)
}

func (b *Bridge) inputs() Inputs {
	return Inputs{
		Services: b.services, Backups: b.backups, Host: b.caps,
		Version: b.version.Version, Commit: b.version.Commit,
		BridgeRev: b.cfg.Rev, DashboardURL: b.cfg.DashboardURL,
	}
}

// publishAll publishes discovery (removing components that vanished since
// the retained payload), marks the bridge online and publishes every state.
// Until the daemon API has answered once, the retained discovery is left
// alone: publishing without the services and backups would delete them.
func (b *Bridge) publishAll(ctx context.Context) {
	if !b.connected.Load() {
		return
	}
	if !b.apiSeen {
		b.try(ctx, topicStatus, []byte("online"), true)
		b.publishState(ctx)
		return
	}
	in := b.inputs()
	idx, err := newIndex(in.Services, in.Backups)
	if err != nil {
		log.Error("cannot build Home Assistant entities", "error", err)
		return
	}
	b.mu.Lock()
	b.idx = idx
	b.mu.Unlock()

	cmps := components(in, idx)
	if gone := removed(b.prev, cmps); len(gone) > 0 {
		withGone := maps.Clone(cmps)
		maps.Copy(withGone, gone)
		log.Info("removing Home Assistant entities", "count", len(gone))
		if !b.try(ctx, b.discoveryTopic(), devicePayload(in, withGone), true) {
			return
		}
	}
	payload := devicePayload(in, cmps)
	if !b.try(ctx, b.discoveryTopic(), payload, true) {
		return
	}
	b.prev = payload
	b.discovered = true
	b.try(ctx, topicStatus, []byte("online"), true)
	b.publishState(ctx)
}

// publishState publishes every entity's current state, slow ones included,
// so an HA that missed a birth still fills in.
func (b *Bridge) publishState(ctx context.Context) {
	if !b.connected.Load() {
		return
	}
	b.try(ctx, topicAPI, []byte(onlineOffline(b.apiOK)), true)
	if s := hostState(b.host.sample(), b.caps, b.host.boot); len(s) > 2 {
		b.try(ctx, topicHost, s, false)
	}
	b.try(ctx, topicLastCommand, []byte(b.lastCmd), false)
	idx := b.currentIndex()
	if idx.services == nil {
		return // discovery has not been published yet
	}
	for _, s := range b.services {
		if k := slug(s.Name); idx.services[k] == s.Name {
			b.try(ctx, svcTopic(k, "state"), []byte(switchState(s.ActiveState, s.SubState)), false)
		}
	}
	for _, bk := range b.backups {
		if k := backupKey(bk); idx.backups[k] == bk.Name {
			b.try(ctx, backupTopic(k, "state"), backupState(bk), false)
		}
	}
}

func onlineOffline(ok bool) string {
	if ok {
		return "online"
	}
	return "offline"
}

// try publishes and reports success; a failure means the connection dropped,
// and the next connect republishes everything.
func (b *Bridge) try(ctx context.Context, topic string, payload []byte, retain bool) bool {
	if err := b.pub.publish(ctx, topic, payload, retain); err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Warn("publish failed", "topic", topic, "error", err)
		}
		return false
	}
	return true
}
