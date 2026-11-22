package bridge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

func TestSwitchState(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ active, sub, want string }{
		{"active", "running", "ON"},
		{"reloading", "reload", "ON"},
		{"activating", "start", "ON"},
		{"activating", "auto-restart", "OFF"},
		{"deactivating", "stop", "OFF"},
		{"inactive", "dead", "OFF"},
		{"failed", "failed", "OFF"},
	} {
		if got := switchState(c.active, c.sub); got != c.want {
			t.Errorf("switchState(%s, %s) = %s, want %s", c.active, c.sub, got, c.want)
		}
	}
}

func TestHostState(t *testing.T) {
	t.Parallel()
	boot := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	all := hostCaps{CPU: true, Mem: true, CPUTemp: true, NVMeTemp: true, Boot: true}
	var got map[string]any
	if err := json.Unmarshal(hostState(hostSample{CPUUsage: ptr(11.3), CPUTemp: ptr(28.0), MemUsage: ptr(34.5)}, all, boot), &got); err != nil {
		t.Fatal(err)
	}
	if got["cpu_usage"] != 11.3 || got["cpu_temp"] != 28.0 || got["mem_usage"] != 34.5 || got["boot_time"] != "2026-10-13T08:00:00Z" {
		t.Fatalf("host state = %v", got)
	}
	// No NVMe reading publishes unknown, never 0.
	if v, ok := got["nvme_temp"]; !ok || v != nil {
		t.Fatalf("nvme_temp = %v (present %v), want null", v, ok)
	}
	// Sensors that do not exist are not in the state at all.
	got = nil
	_ = json.Unmarshal(hostState(hostSample{CPUUsage: ptr(1.0)}, hostCaps{CPU: true}, boot), &got)
	if len(got) != 1 {
		t.Fatalf("host state without sensors = %v", got)
	}
}

func TestBackupState(t *testing.T) {
	t.Parallel()
	var got map[string]any
	if err := json.Unmarshal(backupState(homelab.Backup{ID: 7, Name: "App data"}), &got); err != nil {
		t.Fatal(err)
	}
	if got["result"] != "never" || got["last_run"] != nil || got["next_run"] != nil || got["overdue"] != false {
		t.Fatalf("never-run backup = %v", got)
	}

	end := time.Date(2026, 10, 19, 11, 17, 52, 0, time.UTC)
	next := time.Date(2026, 10, 20, 4, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	_ = json.Unmarshal(backupState(homelab.Backup{LastRun: &homelab.BackupRun{Result: "partial", FinishedAt: &end}, NextRun: &next, Overdue: true}), &got)
	if got["result"] != "partial" || got["last_run"] != "2026-10-19T11:17:52Z" ||
		got["next_run"] != "2026-10-20T04:00:00+02:00" || got["overdue"] != true {
		t.Fatalf("backup state = %v", got)
	}

	_ = json.Unmarshal(backupState(homelab.Backup{LastRun: &homelab.BackupRun{Result: "exploded"}}), &got)
	if got["result"] != "unknown" {
		t.Fatalf("unlisted result = %v, want unknown", got["result"])
	}
}
