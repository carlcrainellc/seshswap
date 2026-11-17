package exits

import (
	"context"
	"time"
)

// Seller-side watchdog. It checks the exit the way a buyer uses it: a small
// probe router on the seller's host, in its own network namespace and let in
// by the exit's whitelist, maps the exit and fetches over it (WebProbe: a
// lookup with the exit's resolver, then a TLS handshake on port 443). Both
// cross the Session Router network, so an answer means the exit router is
// taking sessions and passing buyers' web traffic out of its WAN.
//
// After Fails failed probes in a row the exit router is restarted with the
// same key, so it keeps its .sesh. Before that the watchdog maps the exit
// again and restarts the probe router once, so a broken probe is not blamed
// on the exit. The exit is never restarted before the probe has worked at
// least once since the watchdog started. Restarts are rate-limited, and the
// gap doubles while restarts don't help.
//
// (The router's status line can't do this: an exit that only answers buyers
// always shows zero outbound paths, healthy or not.)

// Watchdog decides when to restart an exit router that stopped answering.
type Watchdog struct {
	Probe        func(context.Context) error // nil when the exit answered
	Remap        func(context.Context) error // map the exit on the probe router
	RestartProbe func(context.Context) error // restart the probe router
	Restart      func(context.Context) error // restart the exit router (same key)

	Fails  int           // failed probes in a row before restarting the exit (default 10)
	Grace  time.Duration // no restart this soon after start or a restart (default 5m)
	MinGap time.Duration // least time between restarts (default 15m)
	MaxGap time.Duration // the gap doubles up to this while restarts don't help (default 2h)
	Log    func(string, ...any)
	Now    func() time.Time

	started        time.Time
	mapped         bool
	everOK         bool
	fails          int
	probeRestarted bool
	lastRestart    time.Time
	gap            time.Duration
	restarts       int
	note           string
	okN, failN     int // since the last hourly report
	reported       time.Time
}

func (w *Watchdog) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watchdog) logf(f string, a ...any) {
	if w.Log != nil {
		w.Log(f, a...)
	}
}

// say logs a state once, until the state changes.
func (w *Watchdog) say(key, f string, a ...any) {
	if w.note == key {
		return
	}
	w.note = key
	w.logf(f, a...)
}

func def(d, v time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return v
}

// Restarts is how many times the watchdog restarted the exit router.
func (w *Watchdog) Restarts() int { return w.restarts }

// Step probes once. It returns what it did: "ok", "unmapped", "failing",
// "remapped", "probe-restarted", "not-yet-ok", "grace", "rate-limited",
// "restarted" or "restart-failed".
func (w *Watchdog) Step(ctx context.Context) string {
	now := w.now()
	if w.started.IsZero() {
		w.started = now
	}
	fails := w.Fails
	if fails <= 0 {
		fails = 10
	}
	grace, minGap, maxGap := def(w.Grace, 5*time.Minute), def(w.MinGap, 15*time.Minute), def(w.MaxGap, 2*time.Hour)
	if w.gap == 0 {
		w.gap = minGap
	}
	if !w.mapped {
		if err := w.Remap(ctx); err != nil {
			w.say("map:"+err.Error(), "watchdog: can't map the exit on the probe router yet: %v", err)
			return "unmapped"
		}
		w.mapped = true
		w.logf("watchdog: exit mapped on the probe router")
	}
	err := w.Probe(ctx)
	if w.reported.IsZero() {
		w.reported = now
	}
	if err == nil {
		w.okN++
	} else {
		w.failN++
	}
	if now.Sub(w.reported) >= time.Hour {
		w.logf("watchdog: last hour: %d probes answered, %d failed; %d restarts so far", w.okN, w.failN, w.restarts)
		w.okN, w.failN, w.reported = 0, 0, now
	}
	if err == nil {
		if w.fails > 0 {
			w.logf("watchdog: the exit answers again (after %d failed probes)", w.fails)
		}
		if !w.everOK {
			w.logf("watchdog: probe through the exit works")
		}
		if !w.lastRestart.IsZero() {
			w.gap = minGap // answering again: the next trouble starts from the short gap
		}
		w.fails, w.probeRestarted, w.everOK = 0, false, true
		w.say("ok", "watchdog: exit healthy")
		return "ok"
	}
	w.fails++
	w.note = ""
	w.logf("watchdog: probe %d/%d failed: %v", w.fails, fails, err)
	switch {
	case w.fails == 2:
		// a lost session on the probe side is the cheap thing to rule out
		if err := w.Remap(ctx); err != nil {
			w.logf("watchdog: re-mapping the exit on the probe router: %v", err)
		}
		return "remapped"
	case w.fails >= (fails+1)/2 && !w.probeRestarted:
		w.probeRestarted = true
		w.logf("watchdog: restarting the probe router, in case it is the broken one")
		if err := w.RestartProbe(ctx); err != nil {
			w.logf("watchdog: probe router restart failed: %v", err)
		}
		w.mapped = false
		return "probe-restarted"
	case w.fails < fails:
		return "failing"
	case !w.everOK:
		w.say("never", "watchdog: the probe has never worked since start, so the exit is not restarted; check the probe router (log/probe.log)")
		return "not-yet-ok"
	case now.Sub(w.started) < grace || (!w.lastRestart.IsZero() && now.Sub(w.lastRestart) < grace):
		return "grace"
	case !w.lastRestart.IsZero() && now.Sub(w.lastRestart) < w.gap:
		w.say("limited", "watchdog: exit still not answering, but the last restart was %s ago; next one allowed after %s",
			now.Sub(w.lastRestart).Round(time.Second), w.lastRestart.Add(w.gap).Format(time.RFC3339))
		return "rate-limited"
	}
	if !w.lastRestart.IsZero() {
		// the previous restart did not fix it: wait longer next time
		w.gap *= 2
		if w.gap > maxGap {
			w.gap = maxGap
		}
	}
	w.logf("watchdog: restarting the exit router: %d probes in a row got no answer (restart %d)", w.fails, w.restarts+1)
	w.lastRestart, w.fails, w.probeRestarted, w.note = now, 0, false, ""
	if err := w.Restart(ctx); err != nil {
		w.logf("watchdog: restart failed: %v", err)
		return "restart-failed"
	}
	w.restarts++
	w.logf("watchdog: exit router restarted; next restart allowed after %s", now.Add(w.gap).Format(time.RFC3339))
	return "restarted"
}

// Run calls Step every interval until ctx ends.
func (w *Watchdog) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.Step(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
