package bridge

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The stack runs in systemd-nspawn with a private user and network
// namespace and no lxcfs, so /proc/stat, /proc/meminfo and /sys/class/hwmon
// show the whole host. The network counters (only the stack's veth) and the
// root disk (the stack's root on /cache, not the host's /) would not, so
// those sensors are gone.

// hostReader samples the host sensors from a root directory ("/" in
// production, a fixture tree in tests). A sensor whose source could not be
// found when the reader was built is absent from discovery and state.
type hostReader struct {
	root     string
	cpuTemp  string // hwmon temp*_input for the CPU package, "" if none
	nvmeTemp string // hwmon temp*_input for the NVMe drive, "" if none
	hasCPU   bool
	hasMem   bool
	boot     time.Time // zero if unknown

	prevBusy, prevTotal uint64
}

// hostSample is one reading. A nil field has no reading this time and is
// published as null, which HA shows as unknown.
type hostSample struct {
	CPUUsage *float64
	CPUTemp  *float64
	NVMeTemp *float64
	MemUsage *float64
}

func newHostReader(root string) *hostReader {
	h := &hostReader{root: root}
	if _, _, err := h.cpuTimes(); err == nil {
		h.hasCPU = true
	}
	if _, err := h.memUsage(); err == nil {
		h.hasMem = true
	}
	if t, err := readBootTime(h.path("proc/stat")); err == nil {
		h.boot = t
	} else {
		log.Warn("boot time unavailable", "error", err)
	}
	h.cpuTemp, h.nvmeTemp = findTemps(h.path("sys/class/hwmon"))
	log.Info("host sensors", "cpu_usage", h.hasCPU, "memory_usage", h.hasMem,
		"cpu_temperature", h.cpuTemp != "", "nvme_temperature", h.nvmeTemp != "", "boot_time", !h.boot.IsZero())
	return h
}

func (h *hostReader) path(p string) string { return filepath.Join(h.root, p) }

// sample reads every available sensor. CPU usage is the busy share since
// the previous sample, so the first sample has none.
func (h *hostReader) sample() hostSample {
	var s hostSample
	if h.hasCPU {
		if busy, total, err := h.cpuTimes(); err == nil {
			if h.prevTotal != 0 && total > h.prevTotal && busy >= h.prevBusy {
				s.CPUUsage = ptr(round1(100 * float64(busy-h.prevBusy) / float64(total-h.prevTotal)))
			}
			h.prevBusy, h.prevTotal = busy, total
		}
	}
	if h.hasMem {
		if v, err := h.memUsage(); err == nil {
			s.MemUsage = ptr(round1(v))
		}
	}
	s.CPUTemp = readTemp(h.cpuTemp)
	s.NVMeTemp = readTemp(h.nvmeTemp)
	return s
}

// cpuTimes returns busy and total jiffies from the aggregate cpu line of
// /proc/stat. Idle and iowait count as idle; guest time is already inside
// user and nice.
func (h *hostReader) cpuTimes() (busy, total uint64, err error) {
	raw, err := os.ReadFile(h.path("proc/stat"))
	if err != nil {
		return 0, 0, err
	}
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	f := strings.Fields(string(line))
	if len(f) < 9 || f[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat cpu line %q", line)
	}
	var v [8]uint64
	for i := range v {
		if v[i], err = strconv.ParseUint(f[i+1], 10, 64); err != nil {
			return 0, 0, err
		}
		total += v[i]
	}
	idle := v[3] + v[4]
	return total - idle, total, nil
}

// memUsage is the share of memory not available to new allocations, from
// MemTotal and MemAvailable.
func (h *hostReader) memUsage() (float64, error) {
	f, err := os.Open(h.path("proc/meminfo"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var total, avail float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 64)
		if err != nil {
			continue
		}
		switch k {
		case "MemTotal":
			total = n
		case "MemAvailable":
			avail = n
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("no MemTotal in /proc/meminfo")
	}
	return 100 * (total - avail) / total, nil
}

// findTemps picks the CPU package sensor (coretemp "Package id 0", else
// the first coretemp/k10temp input) and the NVMe composite sensor (the
// first input of an "nvme" device) under a hwmon class directory.
func findTemps(dir string) (cpu, nvme string) {
	devs, _ := filepath.Glob(filepath.Join(dir, "hwmon*"))
	var cpuFirst string
	for _, d := range devs {
		name, err := os.ReadFile(filepath.Join(d, "name"))
		if err != nil {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		if len(inputs) == 0 {
			continue
		}
		sortTempInputs(inputs)
		switch strings.TrimSpace(string(name)) {
		case "coretemp", "k10temp":
			for _, in := range inputs {
				label, _ := os.ReadFile(strings.TrimSuffix(in, "_input") + "_label")
				l := strings.TrimSpace(string(label))
				if cpu == "" && (l == "Package id 0" || l == "Tctl") && readTemp(in) != nil {
					cpu = in
				}
			}
			if cpuFirst == "" && readTemp(inputs[0]) != nil {
				cpuFirst = inputs[0]
			}
		case "nvme":
			if nvme == "" && readTemp(inputs[0]) != nil {
				nvme = inputs[0]
			}
		}
	}
	if cpu == "" {
		cpu = cpuFirst
	}
	return cpu, nvme
}

// sortTempInputs orders temp1_input, temp2_input, … numerically.
func sortTempInputs(in []string) {
	num := func(p string) int {
		s := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "temp"), "_input")
		n, _ := strconv.Atoi(s)
		return n
	}
	slices.SortFunc(in, func(a, b string) int { return num(a) - num(b) })
}

// readTemp reads a hwmon millidegree file. 0 or an error is no reading.
func readTemp(path string) *float64 {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil || v == 0 {
		return nil
	}
	return ptr(round1(v / 1000))
}

// readBootTime reads the kernel boot time (btime) from /proc/stat. It is
// the host's, not the stack's: the pid namespace does not change it.
func readBootTime(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("no btime in %s", path)
}
