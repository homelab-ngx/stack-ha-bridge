package bridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

// Topics outside the discovery prefix. Everything the bridge publishes or
// takes commands on lives under homelab/, as it did in the daemon.
const (
	topicStatus      = "homelab/status"     // the bridge is connected (retained, also the will)
	topicAPI         = "homelab/api/status" // the daemon API answered the last read (retained)
	topicHost        = "homelab/host/state"
	topicLastCommand = "homelab/last_command/state"
)

func svcTopic(s, leaf string) string    { return "homelab/svc/" + s + "/" + leaf }
func backupTopic(s, leaf string) string { return "homelab/backup/" + s + "/" + leaf }

// backupKey is a backup's part of its topic and entity IDs: its id, which a
// rename does not change.
func backupKey(b homelab.Backup) string { return strconv.FormatInt(b.ID, 10) }

// commandFilters are the subscriptions that receive every command topic
// components() advertises.
func commandFilters() []string {
	return []string{svcTopic("+", "set"), svcTopic("+", "restart")}
}

// backupResults are the values the backup result enum sensor accepts.
var backupResults = []string{"success", "partial", "failure", "cancelled", "running", "unknown", "never"}

// hostCaps says which host sensors could be read when the bridge started.
type hostCaps struct {
	CPU, Mem, CPUTemp, NVMeTemp, Boot bool
}

// Inputs is the state discovery is built from.
type Inputs struct {
	Services     []homelab.Service
	Backups      []homelab.Backup
	Host         hostCaps
	Version      string // the daemon's
	Commit       string // the daemon's
	BridgeRev    string
	DashboardURL string
}

// slug turns a service name into the [a-z0-9_] form used in
// topics and entity IDs.
func slug(name string) string {
	name = strings.TrimSuffix(strings.ToLower(name), ".service")
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// index maps slugs back to the names the API takes, so a command topic
// never feeds raw topic text to an API call.
type index struct {
	services map[string]string // slug → service name
	backups  map[string]string // backup id → its name
}

func newIndex(services []homelab.Service, backups []homelab.Backup) (index, error) {
	idx := index{services: map[string]string{}, backups: map[string]string{}}
	for _, s := range services {
		if s.Stack == "" {
			continue // a core unit: read-only, never a switch
		}
		k := slug(s.Name)
		if prev, dup := idx.services[k]; dup {
			return index{}, fmt.Errorf("services %q and %q share the slug %q", prev, s.Name, k)
		}
		idx.services[k] = s.Name
	}
	for _, b := range backups {
		idx.backups[backupKey(b)] = b.Name
	}
	return idx, nil
}

type component = map[string]any

var (
	// bridgeOnly is the availability of entities the bridge produces itself.
	bridgeOnly = []component{{"topic": topicStatus}}
	// withAPI is the availability of entities read from the daemon: they go
	// unavailable while the API does not answer, though MQTT is up.
	withAPI = []component{{"topic": topicStatus}, {"topic": topicAPI}}
)

// entity starts a component whose unique_id and default_entity_id follow
// the homelab_<id> scheme the daemon used. Both are frozen once HA has seen
// them.
func entity(platform, id, name string) component {
	uid := "homelab_" + id
	return component{
		"platform":          platform,
		"unique_id":         uid,
		"default_entity_id": platform + "." + uid,
		"name":              name,
		"availability":      bridgeOnly,
	}
}

func apiEntity(platform, id, name string) component {
	return with(entity(platform, id, name), "availability", withAPI, "availability_mode", "all")
}

func with(c component, kv ...any) component {
	for i := 0; i+1 < len(kv); i += 2 {
		c[kv[i].(string)] = kv[i+1]
	}
	return c
}

func hostSensor(id, name, field string, kv ...any) component {
	return with(entity("sensor", id, name), append([]any{
		"state_topic", topicHost,
		"value_template", "{{ value_json." + field + " }}",
	}, kv...)...)
}

// components builds every entity for the current state, keyed by unique_id.
func components(in Inputs, idx index) map[string]component {
	pct := []any{"unit_of_measurement", "%", "state_class", "measurement"}
	temp := []any{"device_class", "temperature", "unit_of_measurement", "°C", "state_class", "measurement"}
	list := []component{
		with(entity("sensor", "last_command", "Last command"),
			"state_topic", topicLastCommand, "entity_category", "diagnostic"),
	}
	if in.Host.CPU {
		list = append(list, hostSensor("cpu_usage", "CPU usage", "cpu_usage", pct...))
	}
	if in.Host.CPUTemp {
		list = append(list, hostSensor("cpu_temperature", "CPU temperature", "cpu_temp", temp...))
	}
	if in.Host.NVMeTemp {
		list = append(list, hostSensor("nvme_temperature", "NVMe temperature", "nvme_temp", temp...))
	}
	if in.Host.Mem {
		list = append(list, hostSensor("memory_usage", "Memory usage", "mem_usage", pct...))
	}
	if in.Host.Boot {
		list = append(list, hostSensor("boot_time", "Boot time", "boot_time",
			"device_class", "timestamp", "entity_category", "diagnostic"))
	}
	for s, name := range idx.services {
		list = append(list,
			with(apiEntity("switch", s, name),
				"state_topic", svcTopic(s, "state"), "command_topic", svcTopic(s, "set"),
				"payload_on", "ON", "payload_off", "OFF", "entity_category", "config"),
			with(apiEntity("button", s+"_restart", "Restart "+name),
				"command_topic", svcTopic(s, "restart"), "payload_press", "PRESS",
				"device_class", "restart", "entity_category", "config"),
		)
	}
	for s, backup := range idx.backups {
		name := "Backup " + backup
		id := "backup_" + s
		st := backupTopic(s, "state")
		list = append(list,
			with(apiEntity("sensor", id+"_result", name+" result"),
				"state_topic", st, "value_template", "{{ value_json.result }}",
				"device_class", "enum", "options", backupResults),
			with(apiEntity("sensor", id+"_last_run", name+" last run"),
				"state_topic", st, "value_template", "{{ value_json.last_run }}", "device_class", "timestamp"),
			with(apiEntity("sensor", id+"_next_run", name+" next run"),
				"state_topic", st, "value_template", "{{ value_json.next_run }}", "device_class", "timestamp"),
			with(apiEntity("binary_sensor", id+"_overdue", name+" overdue"),
				"state_topic", st, "value_template", "{{ 'ON' if value_json.overdue else 'OFF' }}",
				"device_class", "problem"),
		)
	}
	out := make(map[string]component, len(list))
	for _, c := range list {
		out[c["unique_id"].(string)] = c
	}
	return out
}

func devicePayload(in Inputs, cmps map[string]component) []byte {
	sw := in.Version
	if in.Commit != "" {
		sw += " (" + in.Commit + ")"
	}
	dev := component{"identifiers": []string{"homelab"}, "name": "homelab", "manufacturer": "homelab"}
	if sw != "" {
		dev["sw_version"] = sw
	}
	if in.DashboardURL != "" {
		dev["configuration_url"] = in.DashboardURL
	}
	origin := component{"name": "homelab-ha"}
	if in.BridgeRev != "" {
		origin["sw_version"] = in.BridgeRev
	}
	b, _ := json.Marshal(component{
		"device":     dev,
		"origin":     origin,
		"qos":        1,
		"components": cmps,
	})
	return b
}

// removed returns platform-only entries for every component in the previous
// retained payload that the current set no longer has. HA removes a device
// component only after seeing it with nothing but its platform.
func removed(prev []byte, cur map[string]component) map[string]component {
	var old struct {
		Components map[string]struct {
			Platform string `json:"platform"`
		} `json:"components"`
	}
	out := map[string]component{}
	if len(prev) == 0 || json.Unmarshal(prev, &old) != nil {
		return out
	}
	for k, c := range old.Components {
		if _, ok := cur[k]; !ok && c.Platform != "" {
			out[k] = component{"platform": c.Platform}
		}
	}
	return out
}
