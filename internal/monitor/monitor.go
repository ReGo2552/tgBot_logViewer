// Package monitor collects Linux metrics without a shell or elevated privileges.
package monitor

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Disk struct {
	Path            string
	Total, Used     uint64
	Percent, Inodes float64
}

type Process struct {
	PID               int
	Name, User, State string
	CPU               float64 // 100% is one CPU core, as in top.
	RSS               uint64
	ticks, start      uint64
}

type Snapshot struct {
	Time                                         time.Time
	Host                                         string
	Uptime                                       time.Duration
	CPU                                          float64
	CPUReady                                     bool
	CPUs                                         int
	Load                                         string
	MemoryTotal, MemoryUsed, SwapTotal, SwapUsed uint64
	MemoryReady                                  bool
	RX, TX                                       float64 // bytes per second, all non-loopback interfaces
	NetworkReady                                 bool
	Disks                                        []Disk
	Processes                                    []Process
	History                                      []float64
	Warnings                                     []string
}

func (s Snapshot) RAMPercent() float64 { return Percent(s.MemoryUsed, s.MemoryTotal) }
func Percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return math.Min(100, float64(used)/float64(total)*100)
}

type Collector struct {
	mu                  sync.RWMutex
	snapshot            Snapshot
	proc                string
	mounts              []string
	total, idle, rx, tx uint64
	previous            time.Time
	processes           map[int]Process
	users               map[uint32]string
}

func New(mounts []string) *Collector {
	return &Collector{proc: "/proc", mounts: mounts, processes: map[int]Process{}, users: map[uint32]string{}}
}

// Snapshot returns independent slices so callers may sort and render concurrently.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snapshot
	s.Disks = append([]Disk(nil), s.Disks...)
	s.Processes = append([]Process(nil), s.Processes...)
	s.History = append([]float64(nil), s.History...)
	s.Warnings = append([]string(nil), s.Warnings...)
	return s
}

// Run is the sole writer. The first sample has no CPU/rate deltas yet.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	c.Sample(time.Now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.Sample(now)
		}
	}
}

func (c *Collector) Sample(now time.Time) {
	s := Snapshot{Time: now, CPUs: runtime.NumCPU()}
	s.Host, _ = os.Hostname()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(c.proc, name))
		if err != nil {
			s.Warnings = append(s.Warnings, name+": "+err.Error())
		}
		return string(b)
	}
	total, idle, cpus, err := parseCPU(read("stat"))
	cpuOK := err == nil
	if err != nil {
		s.Warnings = append(s.Warnings, err.Error())
	}
	if cpus > 0 {
		s.CPUs = cpus
	}
	var delta uint64
	if err == nil && c.total > 0 && !c.previous.IsZero() && total > c.total && idle >= c.idle {
		delta = total - c.total
		s.CPU = 100 * (1 - float64(min(idle-c.idle, delta))/float64(delta))
		s.CPUReady = true
	}
	mem := parseMemory(read("meminfo"))
	s.MemoryTotal = mem["MemTotal"]
	available, ok := mem["MemAvailable"]
	if !ok {
		available = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	s.MemoryUsed = s.MemoryTotal - min(available, s.MemoryTotal)
	s.MemoryReady = s.MemoryTotal > 0
	if !s.MemoryReady {
		s.Warnings = append(s.Warnings, "meminfo: память недоступна")
	}
	s.SwapTotal = mem["SwapTotal"]
	s.SwapUsed = s.SwapTotal - min(mem["SwapFree"], s.SwapTotal)
	if f := strings.Fields(read("uptime")); len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		s.Uptime = time.Duration(v * float64(time.Second))
	}
	if f := strings.Fields(read("loadavg")); len(f) >= 3 {
		s.Load = strings.Join(f[:3], " / ")
	}
	rx, tx, netOK := parseNetwork(read("net/dev"))
	elapsed := now.Sub(c.previous).Seconds()
	if netOK && !c.previous.IsZero() && elapsed > 0 && rx >= c.rx && tx >= c.tx {
		s.RX, s.TX = float64(rx-c.rx)/elapsed, float64(tx-c.tx)/elapsed
		s.NetworkReady = true
	}
	for _, p := range c.mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			s.Warnings = append(s.Warnings, p+": "+err.Error())
			continue
		}
		used := st.Blocks - min(st.Bfree, st.Blocks)
		s.Disks = append(s.Disks, Disk{Path: p, Total: st.Blocks * uint64(st.Bsize), Used: used * uint64(st.Bsize), Percent: Percent(used, used+st.Bavail), Inodes: Percent(st.Files-min(st.Ffree, st.Files), st.Files)})
	}
	entries, err := os.ReadDir(c.proc)
	if err != nil {
		s.Warnings = append(s.Warnings, "processes: "+err.Error())
	}
	next := map[int]Process{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		p, err := parseProcess(string(b))
		if err != nil {
			continue
		}
		if old, ok := c.processes[pid]; ok && old.start == p.start && p.ticks >= old.ticks && delta > 0 {
			p.CPU = float64(p.ticks-old.ticks) / float64(delta) * float64(s.CPUs) * 100
		}
		if st, err := os.Stat(filepath.Join(c.proc, e.Name())); err == nil {
			if raw, ok := st.Sys().(*syscall.Stat_t); ok {
				p.User = c.username(raw.Uid)
			}
		}
		next[pid] = p
		s.Processes = append(s.Processes, p)
	}
	// Deterministic order also makes ties stable between refreshes.
	sort.Slice(s.Processes, func(i, j int) bool { return s.Processes[i].PID < s.Processes[j].PID })
	c.processes = next
	if cpuOK {
		c.total, c.idle = total, idle
	} else {
		c.total, c.idle = 0, 0
	}
	if netOK {
		c.rx, c.tx = rx, tx
	} else {
		c.rx, c.tx = math.MaxUint64, math.MaxUint64
	}
	c.previous = now
	c.mu.Lock()
	s.History = append([]float64(nil), c.snapshot.History...)
	if s.CPUReady {
		s.History = append(s.History, math.Round(s.CPU))
	}
	if len(s.History) > 120 {
		s.History = s.History[len(s.History)-120:]
	}
	c.snapshot = s
	c.mu.Unlock()
}

func (c *Collector) username(uid uint32) string {
	if name, ok := c.users[uid]; ok {
		return name
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	c.users[uid] = name
	return name
}

func parseCPU(text string) (total, idle uint64, cpus int, err error) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			if len(f) < 5 {
				return 0, 0, 0, fmt.Errorf("/proc/stat: incomplete CPU counters")
			}
			// guest/guest_nice are already included in user/nice.
			for i := 1; i < len(f) && i <= 8; i++ {
				n, e := strconv.ParseUint(f[i], 10, 64)
				if e != nil {
					return 0, 0, 0, e
				}
				total += n
				if i == 4 || i == 5 {
					idle += n
				}
			}
		} else if strings.HasPrefix(f[0], "cpu") {
			cpus++
		}
	}
	if total == 0 {
		err = fmt.Errorf("/proc/stat: no CPU counters")
	}
	return
}

func parseMemory(text string) map[string]uint64 {
	m := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 2 && f[2] == "kB" {
			n *= 1024
		}
		m[strings.TrimSuffix(f[0], ":")] = n
	}
	return m
}

func parseNetwork(text string) (rx, tx uint64, ok bool) {
	s := bufio.NewScanner(strings.NewReader(text))
	for s.Scan() {
		name, raw, found := strings.Cut(s.Text(), ":")
		if !found {
			continue
		}
		f := strings.Fields(raw)
		if len(f) < 16 {
			continue
		}
		a, e1 := strconv.ParseUint(f[0], 10, 64)
		b, e2 := strconv.ParseUint(f[8], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		ok = true
		if strings.TrimSpace(name) != "lo" {
			rx += a
			tx += b
		}
	}
	return
}

func parseProcess(text string) (Process, error) {
	left, right := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if left < 1 || right < left {
		return Process{}, fmt.Errorf("invalid process stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:left]))
	if err != nil {
		return Process{}, err
	}
	f := strings.Fields(text[right+1:])
	if len(f) < 22 {
		return Process{}, fmt.Errorf("incomplete process stat")
	}
	u, e1 := strconv.ParseUint(f[11], 10, 64)
	st, e2 := strconv.ParseUint(f[12], 10, 64)
	start, e3 := strconv.ParseUint(f[19], 10, 64)
	rss, e4 := strconv.ParseInt(f[21], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return Process{}, fmt.Errorf("invalid process counters")
	}
	return Process{PID: pid, Name: text[left+1 : right], State: f[0], ticks: u + st, start: start, RSS: uint64(max(rss, 0)) * uint64(os.Getpagesize())}, nil
}
