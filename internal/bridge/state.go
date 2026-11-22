package bridge

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/homelab-ngx/stack-ha-bridge/internal/homelab"
)

// switchState is ON while a unit runs or is starting. A unit waiting out a
// restart delay reports activating/auto-restart; that is a crash loop, so OFF.
func switchState(active, sub string) string {
	switch {
	case active == "active", active == "reloading":
		return "ON"
	case active == "activating" && sub != "auto-restart":
		return "ON"
	}
	return "OFF"
}

func ptr[T any](v T) *T { return &v }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func stamp(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// hostState is the JSON every host sensor reads its value from. Only the
// sensors in caps are present; a missing reading is null (unknown in HA).
func hostState(s hostSample, caps hostCaps, boot time.Time) []byte {
	m := map[string]any{}
	if caps.CPU {
		m["cpu_usage"] = s.CPUUsage
	}
	if caps.CPUTemp {
		m["cpu_temp"] = s.CPUTemp
	}
	if caps.NVMeTemp {
		m["nvme_temp"] = s.NVMeTemp
	}
	if caps.Mem {
		m["mem_usage"] = s.MemUsage
	}
	if caps.Boot {
		m["boot_time"] = stamp(&boot)
	}
	b, _ := json.Marshal(m)
	return b
}

// backupState is the JSON one backup's sensors read from.
func backupState(b homelab.Backup) []byte {
	result := "never"
	var end *time.Time
	if b.LastRun != nil {
		result, end = b.LastRun.Result, b.LastRun.FinishedAt
		if !slices.Contains(backupResults, result) {
			result = "unknown"
		}
	}
	out, _ := json.Marshal(map[string]any{
		"result":   result,
		"last_run": stamp(end),
		"next_run": stamp(b.NextRun),
		"overdue":  b.Overdue,
	})
	return out
}
