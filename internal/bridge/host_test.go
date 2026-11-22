package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixture writes a fake /proc and /sys tree under a temp root.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// hostFiles is the i3-10100's layout as the stack sees it: coretemp with a
// package sensor after an ACPI zone, and the NVMe composite as temp1.
func hostFiles() map[string]string {
	return map[string]string{
		"proc/stat":                           "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3\nbtime 1760342400\n",
		"proc/meminfo":                        "MemTotal:       16000000 kB\nMemFree:  1000 kB\nMemAvailable:    4000000 kB\n",
		"sys/class/hwmon/hwmon0/name":         "nvme\n",
		"sys/class/hwmon/hwmon0/temp1_input":  "38850\n",
		"sys/class/hwmon/hwmon0/temp1_label":  "Composite\n",
		"sys/class/hwmon/hwmon0/temp2_input":  "41850\n",
		"sys/class/hwmon/hwmon1/name":         "acpitz\n",
		"sys/class/hwmon/hwmon1/temp1_input":  "27800\n",
		"sys/class/hwmon/hwmon2/name":         "coretemp\n",
		"sys/class/hwmon/hwmon2/temp10_input": "30000\n",
		"sys/class/hwmon/hwmon2/temp10_label": "Core 9\n",
		"sys/class/hwmon/hwmon2/temp2_input":  "33000\n",
		"sys/class/hwmon/hwmon2/temp2_label":  "Core 0\n",
		"sys/class/hwmon/hwmon2/temp1_input":  "35000\n",
		"sys/class/hwmon/hwmon2/temp1_label":  "Package id 0\n",
	}
}

func TestHostReaderFindsEverySensor(t *testing.T) {
	t.Parallel()
	root := fixture(t, hostFiles())
	h := newHostReader(root)
	if !h.hasCPU || !h.hasMem || !h.boot.Equal(time.Unix(1760342400, 0)) {
		t.Fatalf("reader = %+v", h)
	}
	if h.cpuTemp != filepath.Join(root, "sys/class/hwmon/hwmon2/temp1_input") ||
		h.nvmeTemp != filepath.Join(root, "sys/class/hwmon/hwmon0/temp1_input") {
		t.Fatalf("temps = %s, %s", h.cpuTemp, h.nvmeTemp)
	}
	s := h.sample()
	if s.CPUUsage != nil {
		t.Fatalf("first sample has CPU usage %v, want none", *s.CPUUsage)
	}
	if *s.MemUsage != 75 || *s.CPUTemp != 35 || *s.NVMeTemp != 38.9 {
		t.Fatalf("sample = mem %v cpu %v nvme %v", *s.MemUsage, *s.CPUTemp, *s.NVMeTemp)
	}
	// 100 more busy jiffies out of 400: 25 %.
	_ = os.WriteFile(filepath.Join(root, "proc/stat"), []byte("cpu  150 0 150 950 150 0 0 0 0 0\n"), 0o644)
	if s := h.sample(); s.CPUUsage == nil || *s.CPUUsage != 25 {
		t.Fatalf("cpu usage = %v", s.CPUUsage)
	}
}

func TestHostReaderWithoutSensors(t *testing.T) {
	t.Parallel()
	// No hwmon (or an unreadable one), no meminfo: those sensors are absent.
	h := newHostReader(fixture(t, map[string]string{
		"proc/stat":                          "cpu  1 2 3 4 5 6 7 8 0 0\n",
		"sys/class/hwmon/hwmon0/name":        "coretemp\n",
		"sys/class/hwmon/hwmon0/temp1_input": "0\n",
	}))
	caps := newBridge(Config{}, nil, h).caps
	if caps != (hostCaps{CPU: true}) {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestFindTempsFallsBackToFirstCoretemp(t *testing.T) {
	t.Parallel()
	root := fixture(t, map[string]string{
		"hwmon/hwmon3/name":        "k10temp\n",
		"hwmon/hwmon3/temp3_input": "51000\n",
		"hwmon/hwmon3/temp1_input": "50000\n",
	})
	cpu, nvme := findTemps(filepath.Join(root, "hwmon"))
	if cpu != filepath.Join(root, "hwmon/hwmon3/temp1_input") || nvme != "" {
		t.Fatalf("cpu %q nvme %q", cpu, nvme)
	}
}

func TestReadBootTime(t *testing.T) {
	t.Parallel()
	p := filepath.Join(fixture(t, map[string]string{"stat": "cpu  1 2 3\nintr 5\nbtime 1760342400\nprocesses 9\n"}), "stat")
	got, err := readBootTime(p)
	if err != nil || !got.Equal(time.Unix(1760342400, 0)) {
		t.Fatalf("boot time = %v, %v", got, err)
	}
	if _, err := readBootTime(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file read")
	}
}
