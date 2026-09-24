package cache

import (
	"testing"

	"github.com/gluestick-sh/core/message"
)

func TestGCWorkLedger_noPercentBeforePlanned(t *testing.T) {
	ledger := newGCWorkLedger(func(GCProgressEvent) {})
	ledger.Complete(50)
	if got := ledger.Percent(); got != 0 {
		t.Fatalf("Percent() before planned = %v, want 0", got)
	}
	ledger.AddPlanned(100)
	if got := ledger.Percent(); got != 50 {
		t.Fatalf("Percent() after planned = %v, want 50", got)
	}
}

func TestGCWorkLedger_replanAdjustsDisplay(t *testing.T) {
	var pcts []float64
	ledger := newGCWorkLedger(func(ev GCProgressEvent) {
		pcts = append(pcts, ev.Percent)
	})
	ledger.AddPlanned(10)
	ledger.Complete(8)
	ledger.Report(GCPhaseScan, message.GCScanningStore, nil)
	ledger.AddPlanned(10)
	ledger.Report(GCPhaseScan, message.GCPrepareStore, nil)
	if len(pcts) != 2 {
		t.Fatalf("reports = %d, want 2", len(pcts))
	}
	if pcts[0] != 80 || pcts[1] != 40 {
		t.Fatalf("reports = %v, want [80 40]", pcts)
	}
}

func TestGCWorkLedger_realUnits(t *testing.T) {
	var pcts []float64
	ledger := newGCWorkLedger(func(ev GCProgressEvent) {
		pcts = append(pcts, ev.Percent)
	})

	ledger.AddPlanned(10)
	ledger.Complete(5)
	ledger.Report(GCPhaseScan, message.GCScanningStore, map[string]interface{}{
		"current": 1,
		"total":   2,
	})

	if got := ledger.Percent(); got != 50 {
		t.Fatalf("Percent() = %v, want 50", got)
	}

	ledger.AddPlanned(10)
	for i := 0; i < 10; i++ {
		ledger.Complete(1)
	}
	if got := ledger.Percent(); got != 75 {
		t.Fatalf("Percent() after delete plan = %v, want 75", got)
	}

	ledger.ReportComplete(GCPhaseComplete, message.GCCompleteFreed, nil)
	if pcts[len(pcts)-1] != 100 {
		t.Fatalf("complete pct = %v, want 100", pcts[len(pcts)-1])
	}
}
