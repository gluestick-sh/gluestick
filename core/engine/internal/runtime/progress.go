package runtime

import (
	"context"
	"time"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/message"
)

// OperationContext merges the API context with an optional per-request context.
// This creates a combined context that is cancelled when either the API context
// or the request context is cancelled. This allows long-running operations
// to be cancelled by either global shutdown or per-request cancellation.
// Parameters:
//   - ctx: Global API context (may be nil)
//   - req: Request with optional context (may be nil)
// Returns a merged context that cancels when either input cancels.
func OperationContext(ctx context.Context, req *etypes.Request) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || req.Context == nil {
		return ctx
	}
	if req.Context == ctx {
		return ctx
	}
	merged, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-req.Context.Done():
			cancel()
		case <-merged.Done():
		}
	}()
	return merged
}

// ContextCanceled checks if a context has been cancelled.
// This is a helper to safely check context cancellation without
// dereferencing nil contexts.
// Parameters:
//   - ctx: Context to check (may be nil)
// Returns the context error if cancelled, nil otherwise.
func ContextCanceled(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// ProgressEvent builds a progress event with English message text.
// This creates a structured progress event that includes localized messages
// and all relevant progress information.
// Parameters:
//   - phase: Operation phase (resolve, download, extract, etc.)
//   - pkg: Package reference string
//   - status: Operation status (running, success, failed)
//   - pct: Progress percentage (0-100)
//   - key: Message key for localization
//   - args: Arguments for message formatting
//   - bytes: Bytes processed so far
//   - total: Total bytes to process
// Returns a fully populated progress event with timestamp.
func ProgressEvent(phase etypes.Phase,
	pkg string,
	status etypes.Status,
	pct float64,
	key string,
	args map[string]any,
	bytes, total int64,
) etypes.ProgressEvent {
	payload := message.NewProgress(key, args)
	return etypes.ProgressEvent{
		Phase:       phase,
		Package:     pkg,
		Status:      status,
		Percentage:  pct,
		MessageKey:  payload.MessageKey,
		MessageArgs: payload.MessageArgs,
		Message:     payload.Text(),
		Bytes:       bytes,
		TotalBytes:  total,
		Timestamp:   time.Now(),
	}
}

// ReportProgress emits a progress event when reporter is non-nil.
// This is a helper that safely sends progress updates, handling the case
// where no reporter is configured (e.g., in CLI mode).
// Parameters:
//   - reporter: Progress reporter (may be nil)
//   - phase: Operation phase (resolve, download, extract, etc.)
//   - pkg: Package reference string
//   - status: Operation status (running, success, failed)
//   - pct: Progress percentage (0-100)
//   - key: Message key for localization
//   - args: Arguments for message formatting
//   - bytes: Bytes processed so far
//   - total: Total bytes to process
func ReportProgress(reporter etypes.ProgressReporter,
	phase etypes.Phase,
	pkg string,
	status etypes.Status,
	pct float64,
	key string,
	args map[string]any,
	bytes, total int64,
) {
	if reporter == nil {
		return
	}
	reporter.ReportProgress(ProgressEvent(phase, pkg, status, pct, key, args, bytes, total))
}
