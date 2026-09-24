package cache

import "github.com/gluestick-sh/core/message"

// gcFinalizeScanUnit is one work unit for merging index/apps refs and identifying orphans.
const gcFinalizeScanUnit = int64(1)

// gcProgress reports GC phases using a real work-unit ledger.
type gcProgress struct {
	ledger *gcWorkLedger
}

func newGCProgress(report GCProgressReporter) *gcProgress {
	if report == nil {
		return nil
	}
	return &gcProgress{ledger: newGCWorkLedger(report)}
}

func (p *gcProgress) addPlanned(units int64) {
	if p != nil {
		p.ledger.AddPlanned(units)
	}
}

func (p *gcProgress) complete(units int64) {
	if p != nil {
		p.ledger.Complete(units)
	}
}

func (p *gcProgress) report(phase, key string, args map[string]any) {
	if p != nil {
		p.ledger.Report(phase, key, args)
	}
}

func (p *gcProgress) reportInfo(phase, key string, args map[string]any) {
	if p != nil {
		p.ledger.ReportInfo(phase, key, args)
	}
}

func (p *gcProgress) reportComplete(phase, key string, args map[string]any) {
	if p != nil {
		p.ledger.ReportComplete(phase, key, args)
	}
}

func (p *gcProgress) reportStoreShard(shardDone, shardTotal, objectsScanned int, displayRoot string) {
	if p == nil {
		return
	}
	if shardDone != 1 && shardDone%16 != 0 && shardDone != shardTotal {
		return
	}
	p.report(GCPhaseScan, message.GCScanningStore, map[string]any{
		"scanned": objectsScanned,
		"current": shardDone,
		"total":   shardTotal,
		"root":    displayRoot,
	})
}

func (p *gcProgress) reportAppScan(files, fileTotal, dirsDone, dirTotal, packagesDone, packagesTotal int) {
	if p == nil {
		return
	}
	p.report(GCPhaseScan, message.GCScanningAppsMerged, map[string]any{
		"files":     files,
		"fileTotal": fileTotal,
		"dirsDone":  dirsDone,
		"dirTotal":  dirTotal,
		"current":   packagesDone,
		"total":     packagesTotal,
	})
}

func (p *gcProgress) reportDelete(current, total int, messageKey string) {
	if p == nil {
		return
	}
	p.report(GCPhaseDelete, messageKey, map[string]interface{}{
		"current": current,
		"total":   total,
	})
}

func (p *gcProgress) reportPurgeFiles(pkgName string, scanned int) {
	if p == nil {
		return
	}
	p.report(GCPhaseScan, message.PurgeScanningFiles, map[string]any{
		"package": pkgName,
		"scanned": scanned,
	})
}
