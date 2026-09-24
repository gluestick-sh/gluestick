// Package progress provides context-carried progress callbacks shared across core I/O.
package progress

import "context"

type handlerKey struct{}

// Handler groups byte-, file-, and archive-extract progress callbacks for one operation.
type Handler struct {
	Bytes   func(downloaded, total int64, message string)
	Files   func(processed, total int64)
	Extract func(percent int)
}

// WithHandler attaches progress callbacks to ctx.
func WithHandler(ctx context.Context, h *Handler) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, handlerKey{}, h)
}

// HandlerFrom returns progress callbacks stored on ctx, if any.
func HandlerFrom(ctx context.Context) *Handler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(handlerKey{}).(*Handler)
	return h
}
