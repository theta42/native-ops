package status

import (
	"context"
	"strconv"
	"strings"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// Metrics is a point-in-time read of the host's own load: how busy it is, how much memory and disk it
// has left, and how long it has been up. Read through the same executor as the rest of the snapshot, so
// it works whether the daemon is on the host or driving one over SSH.
type Metrics struct {
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	Cores       int     `json:"cores,omitempty"`
	MemTotalKB  uint64  `json:"mem_total_kb,omitempty"`
	MemAvailKB  uint64  `json:"mem_available_kb,omitempty"`
	SwapTotalKB uint64  `json:"swap_total_kb,omitempty"`
	SwapUsedKB  uint64  `json:"swap_used_kb,omitempty"`
	UptimeSec   uint64  `json:"uptime_sec,omitempty"`
	Disk        *Disk   `json:"disk,omitempty"`
}

// Disk is the storage pool's own filesystem usage.
type Disk struct {
	Mount       string `json:"mount"`
	TotalKB     uint64 `json:"total_kb"`
	UsedKB      uint64 `json:"used_kb"`
	AvailableKB uint64 `json:"available_kb"`
	UsePercent  int    `json:"use_percent"`
}

func parseLoadavg(s string) (float64, float64, float64) {
	f := strings.Fields(s)
	at := func(i int) float64 {
		if i >= len(f) {
			return 0
		}
		v, _ := strconv.ParseFloat(f[i], 64)
		return v
	}
	return at(0), at(1), at(2)
}

// parseMeminfo reads the kB values from /proc/meminfo ("MemTotal:  16384 kB").
func parseMeminfo(s string) map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		// Values are kB except a few (HugePages); those are not used here.
		out[strings.TrimSpace(key)] = v
	}
	return out
}

func parseUptime(s string) uint64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil || secs < 0 {
		return 0
	}
	return uint64(secs)
}

// parseDF reads the first data line of `df -Pk` (portable, 1024-byte blocks):
// "Filesystem 1024-blocks Used Available Capacity Mounted on".
func parseDF(s string) *Disk {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 2 {
		return nil
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 6 {
		return nil
	}
	total, _ := strconv.ParseUint(f[1], 10, 64)
	used, _ := strconv.ParseUint(f[2], 10, 64)
	avail, _ := strconv.ParseUint(f[3], 10, 64)
	pct, _ := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
	return &Disk{Mount: f[5], TotalKB: total, UsedKB: used, AvailableKB: avail, UsePercent: pct}
}

// collectMetrics reads the host's load, memory, uptime and the pool's disk. Every part is best-effort:
// a command that fails leaves that field zero rather than failing the whole snapshot.
func collectMetrics(ctx context.Context, ex remote.Executor, pool string) *Metrics {
	m := &Metrics{}
	if out, err := ex.Run(ctx, "cat /proc/loadavg"); err == nil {
		m.Load1, m.Load5, m.Load15 = parseLoadavg(out)
	}
	if out, err := ex.Run(ctx, "nproc"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			m.Cores = n
		}
	}
	if out, err := ex.Run(ctx, "cat /proc/meminfo"); err == nil {
		kv := parseMeminfo(out)
		m.MemTotalKB, m.MemAvailKB = kv["MemTotal"], kv["MemAvailable"]
		m.SwapTotalKB = kv["SwapTotal"]
		if kv["SwapTotal"] > kv["SwapFree"] {
			m.SwapUsedKB = kv["SwapTotal"] - kv["SwapFree"]
		}
	}
	if out, err := ex.Run(ctx, "cat /proc/uptime"); err == nil {
		m.UptimeSec = parseUptime(out)
	}
	if incus.ValidName(pool) {
		path := "/var/lib/incus/storage-pools/" + pool
		if out, err := ex.Run(ctx, "df -Pk "+incus.ShQuote(path)); err == nil {
			m.Disk = parseDF(out)
		}
	}
	return m
}
