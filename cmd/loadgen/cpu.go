package main

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ClientCPU は、測りの間の負荷の側の CPU（Linux だけ。負荷の側が詰まっていないことを示す）。
type ClientCPU struct {
	ProcessCPUS    float64 `json:"process_cpu_s"`
	ProcessCPUPct  float64 `json:"process_cpu_pct"` // CPU 1 つを 100 とした値
	HostBusyPct    float64 `json:"host_busy_pct"`   // ホストの全部の CPU を 100 とした値
	HostCPUs       int     `json:"host_cpus"`
	ElapsedSeconds float64 `json:"elapsed_s"`
}

type cpuSnap struct {
	ok          bool
	at          time.Time
	proc        float64 // 秒
	total, idle int64   // /proc/stat の tick
}

// clkTck は、/proc の tick の数（Linux ではほぼ 100 に決まっている）。
const clkTck = 100

func readClientCPU() cpuSnap {
	s := cpuSnap{at: time.Now()}
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return s
	}
	str := string(b)
	f := strings.Fields(str[strings.LastIndex(str, ")")+2:])
	if len(f) < 13 {
		return s
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	k, _ := strconv.ParseInt(f[12], 10, 64)
	s.proc = float64(u+k) / clkTck
	st, err := os.ReadFile("/proc/stat")
	if err != nil {
		return s
	}
	line, _, _ := strings.Cut(string(st), "\n")
	fs := strings.Fields(line)
	if len(fs) < 6 {
		return s
	}
	for i, x := range fs[1:] {
		v, _ := strconv.ParseInt(x, 10, 64)
		s.total += v
		if i == 3 || i == 4 { // idle、iowait
			s.idle += v
		}
	}
	s.ok = true
	return s
}

func (a cpuSnap) until(b cpuSnap) *ClientCPU {
	if !a.ok || !b.ok {
		return nil
	}
	el := b.at.Sub(a.at).Seconds()
	r := func(x float64) float64 { return math.Round(x*10) / 10 }
	busy := 0.0
	if b.total > a.total {
		busy = 100 * (1 - float64(b.idle-a.idle)/float64(b.total-a.total))
	}
	return &ClientCPU{ProcessCPUS: r(b.proc - a.proc), ProcessCPUPct: r(100 * (b.proc - a.proc) / el),
		HostBusyPct: r(busy), HostCPUs: runtime.NumCPU(), ElapsedSeconds: r(el)}
}
