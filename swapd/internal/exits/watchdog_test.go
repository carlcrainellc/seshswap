package exits

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type wdHarness struct {
	now                      time.Time
	down                     bool // the exit doesn't answer
	probeBroken              bool // the probe router doesn't work (until restarted)
	restarts, probeRestarts  int
	remaps                   int
	restartFixes, probeFixes bool
	logs                     []string
	w                        *Watchdog
}

func newWD() *wdHarness {
	h := &wdHarness{now: time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC), restartFixes: true, probeFixes: true}
	h.w = &Watchdog{
		Probe: func(context.Context) error {
			if h.probeBroken || h.down {
				return errors.New("no answer")
			}
			return nil
		},
		Remap: func(context.Context) error { h.remaps++; return nil },
		RestartProbe: func(context.Context) error {
			h.probeRestarts++
			if h.probeFixes {
				h.probeBroken = false
			}
			return nil
		},
		Restart: func(context.Context) error {
			h.restarts++
			if h.restartFixes {
				h.down = false
			}
			return nil
		},
		Fails: 10, Grace: 5 * time.Minute, MinGap: 15 * time.Minute, MaxGap: time.Hour,
		Now: func() time.Time { return h.now },
		Log: func(f string, a ...any) { h.logs = append(h.logs, fmt.Sprintf(f, a...)) },
	}
	return h
}

func (h *wdHarness) run(d, step time.Duration) (last string) {
	for end := h.now.Add(d); !h.now.After(end); h.now = h.now.Add(step) {
		last = h.w.Step(context.Background())
	}
	return last
}

func TestWatchdogHealthyIdleNeverRestarts(t *testing.T) {
	h := newWD()
	if got := h.run(24*time.Hour, 30*time.Second); got != "ok" || h.restarts != 0 || h.probeRestarts != 0 {
		t.Fatalf("%s restarts=%d probe=%d", got, h.restarts, h.probeRestarts)
	}
	if h.remaps != 1 {
		t.Fatalf("mapped %d times", h.remaps)
	}
	// three lines at start, then one summary an hour
	if n := len(h.logs); n != 3+24 || !strings.Contains(h.logs[3], "120 probes answered, 0 failed") &&
		!strings.Contains(h.logs[3], "121 probes answered, 0 failed") {
		t.Fatalf("got %d lines: %v", n, h.logs)
	}
}

func TestWatchdogRestartsStuckExit(t *testing.T) {
	h := newWD()
	h.run(10*time.Minute, 30*time.Second)
	h.down = true
	// 2: remap; 5: probe router restart (and remap); 10: exit restart
	for i, want := range []string{"failing", "remapped", "failing", "failing", "probe-restarted", "failing", "failing", "failing", "failing"} {
		h.now = h.now.Add(30 * time.Second)
		if got := h.w.Step(context.Background()); got != want {
			t.Fatalf("step %d: %s, want %s; %v", i+1, got, want, h.logs)
		}
	}
	h.now = h.now.Add(30 * time.Second)
	if got := h.w.Step(context.Background()); got != "restarted" || h.restarts != 1 || h.probeRestarts != 1 {
		t.Fatalf("%s restarts=%d probe=%d", got, h.restarts, h.probeRestarts)
	}
	if got := h.run(time.Hour, 30*time.Second); got != "ok" || h.restarts != 1 {
		t.Fatalf("after restart: %s %d", got, h.restarts)
	}
}

func TestWatchdogBrokenProbeIsNotBlamedOnExit(t *testing.T) {
	h := newWD()
	h.run(10*time.Minute, 30*time.Second)
	h.probeBroken = true // the probe router stalls; restarting it fixes it
	h.run(10*time.Minute, 30*time.Second)
	if h.restarts != 0 || h.probeRestarts != 1 {
		t.Fatalf("restarts=%d probe=%d; %v", h.restarts, h.probeRestarts, h.logs)
	}
}

func TestWatchdogNeverRestartsBeforeFirstAnswer(t *testing.T) {
	h := newWD()
	h.down = true
	h.restartFixes = false
	if got := h.run(time.Hour, 30*time.Second); got != "not-yet-ok" || h.restarts != 0 {
		t.Fatalf("%s %d", got, h.restarts)
	}
}

func TestWatchdogRateLimitAndBackoff(t *testing.T) {
	h := newWD()
	h.run(10*time.Minute, 30*time.Second)
	h.down, h.restartFixes = true, false // restarts don't help
	h.run(6*time.Hour, 30*time.Second)
	// gaps 15m, 30m, 60m, 60m...: at most ~8 restarts in 6h, never two within 15m
	if h.restarts < 3 || h.restarts > 9 {
		t.Fatalf("restarts %d", h.restarts)
	}
	if !strings.Contains(strings.Join(h.logs, "\n"), "next one allowed after") {
		t.Fatal("rate limit not logged")
	}
}

func TestWatchdogGrace(t *testing.T) {
	h := newWD()
	h.run(time.Minute, 30*time.Second) // works once
	h.down = true
	h.run(3*time.Minute, 30*time.Second)
	if h.restarts != 0 {
		t.Fatal("restarted inside the start grace")
	}
	h.run(5*time.Minute, 30*time.Second)
	if h.restarts != 1 {
		t.Fatalf("want a restart after grace, got %d", h.restarts)
	}
}

func TestWatchdogWaitsForMapping(t *testing.T) {
	h := newWD()
	h.w.Remap = func(context.Context) error { return errors.New("router not up") }
	if got := h.w.Step(context.Background()); got != "unmapped" {
		t.Fatal(got)
	}
}
