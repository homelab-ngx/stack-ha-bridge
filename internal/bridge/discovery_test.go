package bridge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

var allHost = hostCaps{CPU: true, Mem: true, CPUTemp: true, NVMeTemp: true, Boot: true}

func svc(name string) homelab.Service {
	return homelab.Service{Name: name, Stack: "media"}
}

func inputs(services ...homelab.Service) Inputs {
	return Inputs{
		Services:     services,
		Backups:      []homelab.Backup{{ID: 7, Name: "App data"}},
		Host:         allHost,
		Version:      "0.2.0",
		Commit:       "abc1234",
		BridgeRev:    "f00ba12",
		DashboardURL: "https://homelab.example.com",
	}
}

type device struct {
	Device     map[string]any            `json:"device"`
	Origin     map[string]any            `json:"origin"`
	Components map[string]map[string]any `json:"components"`
}

// decode decodes a device payload.
func decode(t *testing.T, payload []byte) device {
	t.Helper()
	var dev device
	if err := json.Unmarshal(payload, &dev); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if dev.Device == nil || dev.Origin == nil {
		t.Fatalf("payload lacks device or origin: %s", payload)
	}
	return dev
}

func discover(t *testing.T, in Inputs) device {
	t.Helper()
	idx, err := newIndex(in.Services, in.Backups)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, devicePayload(in, components(in, idx)))
}

func TestSlug(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"calibre-web":   "calibre_web",
		"Immich-Server": "immich_server",
		"plex":          "plex",
	} {
		if got := slug(name); got != want {
			t.Errorf("slug(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestNewIndexRejectsSlugCollisions(t *testing.T) {
	t.Parallel()
	if _, err := newIndex([]homelab.Service{svc("foo-bar"), svc("foo_bar")}, nil); err == nil {
		t.Fatal("two services slugging to foo_bar were accepted")
	}
}

func TestNewIndexSkipsCoreUnits(t *testing.T) {
	t.Parallel()
	idx, err := newIndex([]homelab.Service{svc("plex"), {Name: "caddy"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.services["caddy"]; ok {
		t.Fatal("core unit caddy got an entity")
	}
	if idx.services["plex"] != "plex" {
		t.Fatalf("services = %v", idx.services)
	}
}

func TestDiscoveryDevice(t *testing.T) {
	t.Parallel()
	dev := discover(t, inputs())
	if dev.Device["name"] != "homelab" || dev.Device["sw_version"] != "0.2.0 (abc1234)" ||
		dev.Device["configuration_url"] != "https://homelab.example.com" ||
		!reflect.DeepEqual(dev.Device["identifiers"], []any{"homelab"}) {
		t.Fatalf("device = %v", dev.Device)
	}
	if dev.Origin["name"] != "homelab-ha" || dev.Origin["sw_version"] != "f00ba12" {
		t.Fatalf("origin = %v", dev.Origin)
	}
}

func TestDiscoveryServiceEntities(t *testing.T) {
	t.Parallel()
	cmps := discover(t, inputs(svc("plex"), svc("calibre-web"), homelab.Service{Name: "caddy"})).Components

	sw := cmps["homelab_calibre_web"]
	if sw["platform"] != "switch" || sw["default_entity_id"] != "switch.homelab_calibre_web" ||
		sw["unique_id"] != "homelab_calibre_web" || sw["entity_category"] != "config" ||
		sw["command_topic"] != "homelab/svc/calibre_web/set" || sw["state_topic"] != "homelab/svc/calibre_web/state" {
		t.Fatalf("calibre-web switch = %v", sw)
	}
	btn := cmps["homelab_calibre_web_restart"]
	if btn["platform"] != "button" || btn["device_class"] != "restart" || btn["entity_category"] != "config" ||
		btn["command_topic"] != "homelab/svc/calibre_web/restart" {
		t.Fatalf("calibre-web restart = %v", btn)
	}
	for key := range cmps {
		if key == "homelab_caddy" || key == "homelab_caddy_restart" {
			t.Fatalf("core unit caddy has component %s", key)
		}
	}
}

func TestDiscoveryAvailability(t *testing.T) {
	t.Parallel()
	cmps := discover(t, inputs(svc("plex"))).Components
	bridge := []any{map[string]any{"topic": "homelab/status"}}
	api := []any{map[string]any{"topic": "homelab/status"}, map[string]any{"topic": "homelab/api/status"}}
	for _, k := range []string{"homelab_cpu_usage", "homelab_boot_time", "homelab_last_command"} {
		if !reflect.DeepEqual(cmps[k]["availability"], bridge) || cmps[k]["availability_mode"] != nil {
			t.Errorf("%s availability = %v", k, cmps[k]["availability"])
		}
	}
	for _, k := range []string{"homelab_plex", "homelab_plex_restart", "homelab_backup_7_result", "homelab_backup_7_overdue"} {
		if !reflect.DeepEqual(cmps[k]["availability"], api) || cmps[k]["availability_mode"] != "all" {
			t.Errorf("%s availability = %v mode %v", k, cmps[k]["availability"], cmps[k]["availability_mode"])
		}
	}
}

func TestDiscoveryCategories(t *testing.T) {
	t.Parallel()
	cmps := discover(t, inputs(svc("plex"))).Components
	for _, k := range []string{"homelab_cpu_usage", "homelab_memory_usage", "homelab_cpu_temperature", "homelab_backup_7_overdue"} {
		if c, ok := cmps[k]; !ok || c["entity_category"] != nil {
			t.Errorf("%s = %v, want a primary entity", k, c)
		}
	}
	for _, k := range []string{"homelab_boot_time", "homelab_last_command"} {
		if cmps[k]["entity_category"] != "diagnostic" {
			t.Errorf("%s category = %v, want diagnostic", k, cmps[k]["entity_category"])
		}
	}
	if cmps["homelab_backup_7_result"]["options"] == nil {
		t.Error("backup result enum has no options")
	}
}

// The daemon published these; the stack does not, so the diff removes them.
var droppedEntities = []string{
	"homelab_disk_usage", "homelab_network_in", "homelab_network_out",
	"homelab_pc", "homelab_pc_wake", "homelab_pc_shutdown", "homelab_backup_b2_backup_run",
}

func TestDiscoveryDropsDaemonOnlyEntities(t *testing.T) {
	t.Parallel()
	cmps := discover(t, inputs(svc("plex"))).Components
	for _, k := range droppedEntities {
		if _, ok := cmps[k]; ok {
			t.Errorf("%s still published", k)
		}
	}
	for k, c := range cmps {
		if c["platform"] == "button" && !strings.HasSuffix(k, "_restart") {
			t.Errorf("button %s is not a service restart", k)
		}
	}
}

func TestDiscoveryHostSensorsFollowCaps(t *testing.T) {
	t.Parallel()
	in := inputs()
	in.Host = hostCaps{CPU: true, Mem: true}
	cmps := discover(t, in).Components
	for _, k := range []string{"homelab_cpu_temperature", "homelab_nvme_temperature", "homelab_boot_time"} {
		if _, ok := cmps[k]; ok {
			t.Errorf("%s published without its sensor", k)
		}
	}
	if cmps["homelab_cpu_usage"] == nil || cmps["homelab_memory_usage"] == nil {
		t.Error("readable sensors missing")
	}
}

func TestDiscoveryIdentityIsUniqueAndMatchesEntityID(t *testing.T) {
	t.Parallel()
	var many []homelab.Service
	for i := range 60 {
		many = append(many, svc(fmt.Sprintf("svc-%d", i)))
	}
	cmps := discover(t, inputs(many...)).Components
	seen := map[string]bool{}
	for key, c := range cmps {
		uid, _ := c["unique_id"].(string)
		if uid == "" || uid != key {
			t.Fatalf("component %s unique_id = %q", key, uid)
		}
		if seen[uid] {
			t.Fatalf("duplicate unique_id %s", uid)
		}
		seen[uid] = true
		want := fmt.Sprintf("%s.%s", c["platform"], uid)
		if c["default_entity_id"] != want {
			t.Fatalf("%s default_entity_id = %v, want %s", key, c["default_entity_id"], want)
		}
	}
	// 2 per service, 5 host, 4 per backup, last command.
	if want := 60*2 + 5 + 4 + 1; len(cmps) != want {
		t.Fatalf("%d components, want %d", len(cmps), want)
	}
}

func TestRemovedComponents(t *testing.T) {
	t.Parallel()
	prev := []byte(`{"components":{"homelab_a":{"platform":"switch"},"homelab_b":{"platform":"switch"},"homelab_b_restart":{"platform":"button"},"homelab_c":{"platform":"switch"}}}`)
	cur := map[string]map[string]any{"homelab_a": {}, "homelab_c": {}}
	got := removed(prev, cur)
	want := map[string]map[string]any{
		"homelab_b":         {"platform": "switch"},
		"homelab_b_restart": {"platform": "button"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
	if got := removed(nil, cur); len(got) != 0 {
		t.Fatalf("removed with no previous payload = %v", got)
	}
	if got := removed([]byte("not json"), cur); len(got) != 0 {
		t.Fatalf("removed with a broken payload = %v", got)
	}
}

// matches reports whether an MQTT topic matches a subscription filter that
// uses single-level wildcards.
func matches(filter, topic string) bool {
	f, tp := strings.Split(filter, "/"), strings.Split(topic, "/")
	if len(f) != len(tp) {
		return false
	}
	for i := range f {
		if f[i] != "+" && f[i] != tp[i] {
			return false
		}
	}
	return true
}

func TestCommandFiltersCoverEveryCommandTopic(t *testing.T) {
	t.Parallel()
	in := inputs(svc("plex"), svc("calibre-web"))
	idx, err := newIndex(in.Services, in.Backups)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for key, c := range components(in, idx) {
		topic, ok := c["command_topic"].(string)
		if !ok {
			continue
		}
		n++
		if !slices.ContainsFunc(commandFilters(), func(f string) bool { return matches(f, topic) }) {
			t.Errorf("%s command topic %s matches no subscription", key, topic)
		}
	}
	if n != 4 {
		t.Fatalf("%d command topics, want 4", n)
	}
}
