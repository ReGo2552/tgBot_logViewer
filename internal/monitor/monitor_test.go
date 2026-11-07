package monitor

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func processStat(pid int, name string, ticks, start, rss int) string {
	f := make([]string, 30)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[11] = fmt.Sprint(ticks)
	f[19] = fmt.Sprint(start)
	f[21] = fmt.Sprint(rss)
	return fmt.Sprintf("%d (%s) %s", pid, name, strings.Join(f, " "))
}

func TestProcessNamesAndPIDReuse(t *testing.T) {
	p, err := parseProcess(processStat(42, "odd ) (name", 50, 100, 3))
	if err != nil || p.Name != "odd ) (name" || p.ticks != 50 || p.start != 100 || p.RSS != uint64(3*os.Getpagesize()) {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{"", "42 (foo)", "42 foo x", processStat(42, "x", -1, 1, 1)} {
		if _, err := parseProcess(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestCPUGuestNotDoubleCounted(t *testing.T) {
	total, idle, cpus, err := parseCPU("cpu 100 10 20 300 30 2 3 4 50 5\ncpu0 0\ncpu1 0\n")
	if err != nil || total != 469 || idle != 330 || cpus != 2 {
		t.Fatalf("%d %d %d %v", total, idle, cpus, err)
	}
}

func TestCollectorDeltasMissingDataAndCopies(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 300 kB\nSwapTotal: 100 kB\nSwapFree: 80 kB\n")
	write("uptime", "3600.00 200.00")
	write("loadavg", "1.00 2.00 3.00 1/10 12")
	write("stat", "cpu 100 0 0 900 0 0 0 0\ncpu0 0\ncpu1 0\n")
	write("net/dev", "lo: 9000 0 0 0 0 0 0 0 9000 0 0 0 0 0 0 0\neth0: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n")
	write("42/stat", processStat(42, "my app", 10, 100, 5))
	c := New(nil)
	c.proc = root
	now := time.Now()
	c.Sample(now)
	if s := c.Snapshot(); s.CPUReady || s.NetworkReady || s.RAMPercent() != 70 || s.SwapUsed != 20*1024 {
		t.Fatalf("first %+v", s)
	}
	write("stat", "cpu 120 0 0 980 0 0 0 0\ncpu0 0\ncpu1 0\n")
	write("net/dev", "eth0: 300 0 0 0 0 0 0 0 600 0 0 0 0 0 0 0\n")
	write("42/stat", processStat(42, "my app", 20, 100, 5))
	c.Sample(now.Add(2 * time.Second))
	s := c.Snapshot()
	if !s.CPUReady || math.Abs(s.CPU-20) > .001 || s.Processes[0].CPU != 20 || s.RX != 100 || s.TX != 200 {
		t.Fatalf("deltas %+v", s)
	}
	s.Processes[0].Name = "mutated"
	s.History[0] = 99
	if s := c.Snapshot(); s.Processes[0].Name != "my app" || s.History[0] != 20 {
		t.Fatal("snapshot aliases collector")
	}
	write("42/stat", processStat(42, "reused pid", 200, 999, 1))
	write("stat", "cpu 130 0 0 1070 0 0 0 0\ncpu0 0\ncpu1 0\n")
	c.Sample(now.Add(4 * time.Second))
	if c.Snapshot().Processes[0].CPU != 0 {
		t.Fatal("PID reuse produced fake CPU spike")
	}
	os.Remove(filepath.Join(root, "stat"))
	os.Remove(filepath.Join(root, "net/dev"))
	c.Sample(now.Add(6 * time.Second))
	if s := c.Snapshot(); s.CPUReady || s.NetworkReady || len(s.Warnings) < 2 {
		t.Fatal("missing counters treated as valid")
	}
	write("stat", "cpu 150 0 0 1250 0 0 0 0\ncpu0 0\ncpu1 0\n")
	write("net/dev", "eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n")
	c.Sample(now.Add(8 * time.Second))
	if s := c.Snapshot(); s.CPUReady || s.NetworkReady {
		t.Fatal("recovered counters require a baseline")
	}
}
