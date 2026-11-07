package alerts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSustainedThresholdRecoveryAndRestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "alerts.json")
	e, err := Open(p, 20*time.Second, 10*time.Second, []int64{1, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	bad := []Observation{{Key: "cpu", Title: "CPU: 95%", Bad: true}}
	for _, delta := range []time.Duration{0, 10 * time.Second} {
		if err := e.Evaluate(now.Add(delta), bad); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.Pending(now)) != 0 {
		t.Fatal("short spike triggered")
	}
	if err := e.Evaluate(now.Add(20*time.Second), bad); err != nil {
		t.Fatal(err)
	}
	if len(e.Pending(now)) != 2 {
		t.Fatal("must notify unique admins once")
	}
	e, err = Open(p, 20*time.Second, 10*time.Second, []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate(now.Add(30*time.Second), bad); err != nil {
		t.Fatal(err)
	}
	if len(e.Pending(now)) != 2 {
		t.Fatal("duplicate on restart")
	}
	// In the hysteresis band neither bad nor clear; the incident stays active.
	e.Evaluate(now.Add(40*time.Second), []Observation{{Key: "cpu", Title: "CPU: 88%"}})
	e.Evaluate(now.Add(50*time.Second), nil)
	active, _, _ := e.View(1)
	if len(active) != 1 {
		t.Fatal("missing data claimed recovery")
	}
	e.Evaluate(now.Add(60*time.Second), []Observation{{Key: "cpu", Title: "CPU: 80%", Clear: true}})
	active, history, _ := e.View(1)
	if len(active) != 0 || len(history) != 2 || !history[1].Recovered || len(e.Pending(now)) != 4 {
		t.Fatal("recovery not emitted")
	}
	for _, d := range e.Pending(now) {
		if err := e.Ack(d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.Pending(now)) != 0 {
		t.Fatal("ack")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("state permissions", st.Mode())
	}
}

func TestMissingSampleAndLongGapResetHold(t *testing.T) {
	e, err := Open(filepath.Join(t.TempDir(), "a.json"), 20*time.Second, 10*time.Second, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	bad := []Observation{{Key: "cpu", Bad: true}}
	e.Evaluate(now, bad)
	e.Evaluate(now.Add(10*time.Second), nil)
	e.Evaluate(now.Add(20*time.Second), bad)
	if len(e.Pending(now)) != 0 {
		t.Fatal("missing sample counted towards hold")
	}
	e.Evaluate(now.Add(time.Hour), bad)
	if len(e.Pending(now)) != 0 {
		t.Fatal("downtime counted towards hold")
	}
}

func TestMutePersistsAndClearsQueuedNotifications(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.json")
	now := time.Now()
	e, _ := Open(p, time.Second, time.Second, []int64{1, 2})
	bad := []Observation{{Key: "cpu", Bad: true}}
	e.Evaluate(now, bad)
	e.Evaluate(now.Add(time.Second), bad)
	if err := e.Mute(1, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e, err := Open(p, time.Second, time.Second, []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	_, _, muted := e.View(1)
	if !muted.After(now) {
		t.Fatal("mute lost on restart")
	}
	for _, d := range e.Pending(now.Add(2 * time.Hour)) {
		if d.ChatID == 1 {
			t.Fatal("old notification survived mute")
		}
	}
	if len(e.Pending(now)) != 1 {
		t.Fatal("mute affected other admin")
	}
}

func TestPersistenceFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	e, err := Open(p, time.Second, time.Second, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	// A directory in place of the target makes the atomic rename fail, even as root.
	os.Remove(p)
	os.Mkdir(p, 0o700)
	if err := e.Mute(1, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("save should fail")
	}
	_, _, muted := e.View(1)
	if !muted.IsZero() {
		t.Fatal("failed mutation persisted in memory")
	}
	if err := e.Evaluate(time.Now(), []Observation{{Key: "new", Bad: true}}); err == nil {
		t.Fatal("save should fail")
	}
	if len(e.data.Incidents) != 0 {
		t.Fatal("rollback merged stale map entries")
	}
}
