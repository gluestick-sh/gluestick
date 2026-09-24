package cache

import (
	"sync"
	"sync/atomic"
)

// gcWorkLedger tracks GC progress as completed work units over a planned total.
// Percent is always done/max(planned,done)*100 — no hardcoded phase bands.
type gcWorkLedger struct {
	inner GCProgressReporter
	mu    sync.Mutex

	planned atomic.Int64
	done    atomic.Int64

	lastReportDone int64
	lastEmittedPct float64
}

func newGCWorkLedger(report GCProgressReporter) *gcWorkLedger {
	if report == nil {
		return nil
	}
	return &gcWorkLedger{inner: report}
}

func (w *gcWorkLedger) AddPlanned(units int64) {
	if w == nil || units <= 0 {
		return
	}
	w.planned.Add(units)
	w.mu.Lock()
	if pct := w.percentUnlocked(); pct < w.lastEmittedPct {
		w.lastEmittedPct = pct
	}
	w.mu.Unlock()
}

func (w *gcWorkLedger) Complete(units int64) {
	if w == nil || units <= 0 {
		return
	}
	w.done.Add(units)
	w.maybeReport()
}

func (w *gcWorkLedger) Percent() float64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.percentUnlocked()
}

func (w *gcWorkLedger) percentUnlocked() float64 {
	planned := w.planned.Load()
	if planned == 0 {
		return 0
	}
	done := w.done.Load()
	if done > planned {
		done = planned
	}
	return float64(done) / float64(planned) * 100
}

func (w *gcWorkLedger) Done() int64 {
	if w == nil {
		return 0
	}
	return w.done.Load()
}

func (w *gcWorkLedger) Planned() int64 {
	if w == nil {
		return 0
	}
	planned := w.planned.Load()
	done := w.done.Load()
	if planned < done {
		return done
	}
	return planned
}

func (w *gcWorkLedger) Report(phase, key string, args map[string]interface{}) {
	if w == nil {
		return
	}
	w.emit(phase, key, args, w.Percent())
}

// ReportInfo emits a status message without recalculating percent from the ledger.
func (w *gcWorkLedger) ReportInfo(phase, key string, args map[string]any) {
	if w == nil {
		return
	}
	w.mu.Lock()
	pct := w.lastEmittedPct
	w.mu.Unlock()
	reportGC(w.inner, phase, key, args, pct)
}

func (w *gcWorkLedger) ReportComplete(phase, key string, args map[string]any) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.planned.Store(w.done.Load())
	w.lastEmittedPct = 100
	w.mu.Unlock()
	reportGC(w.inner, phase, key, args, 100)
}

func (w *gcWorkLedger) emit(phase, key string, args map[string]any, pct float64) {
	w.mu.Lock()
	if pct < w.lastEmittedPct {
		pct = w.lastEmittedPct
	} else {
		w.lastEmittedPct = pct
	}
	w.mu.Unlock()
	reportGC(w.inner, phase, key, args, pct)
}

func (w *gcWorkLedger) maybeReport() {
	done := w.done.Load()
	w.mu.Lock()
	defer w.mu.Unlock()
	if done == 1 || done-w.lastReportDone >= 50 || done == w.planned.Load() {
		w.lastReportDone = done
	}
}
