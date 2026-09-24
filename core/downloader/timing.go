package downloader

import "time"

// Timing breaks down a download into network transfer and cache store ingest.
// For HTTP→cache store direct stream, ingest is pipelined with the read: StoreIngest is 0
// and Network covers the full operation.
type Timing struct {
	Network     time.Duration
	StoreIngest time.Duration
}

// addNetwork adds the given duration to the Network timing field.
// Safely handles nil receiver by doing nothing.
func (t *Timing) addNetwork(d time.Duration) {
	if t == nil {
		return
	}
	t.Network += d
}

// addStoreIngest adds the given duration to the StoreIngest timing field.
// Safely handles nil receiver by doing nothing.
func (t *Timing) addStoreIngest(d time.Duration) {
	if t == nil {
		return
	}
	t.StoreIngest += d
}
