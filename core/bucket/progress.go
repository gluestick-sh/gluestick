package bucket

import "github.com/gluestick-sh/core/message"

// BucketProgressEvent is a structured bucket operation progress update for GUI i18n.
type BucketProgressEvent struct {
	Phase           string
	MessageKey      string
	MessageArgs     map[string]interface{}
	MessageFallback string // raw text when MessageKey is empty (e.g. git stderr)
	Percent         float64
}

// BucketProgressReporter receives bucket add/update progress.
type BucketProgressReporter func(BucketProgressEvent)

func reportBucket(report BucketProgressReporter, phase, key string, args map[string]interface{}, pct float64) {
	if report == nil {
		return
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	report(BucketProgressEvent{
		Phase:       phase,
		MessageKey:  key,
		MessageArgs: args,
		Percent:     pct,
	})
}

func reportBucketFallback(report BucketProgressReporter, phase, fallback string, pct float64) {
	if report == nil || fallback == "" {
		return
	}
	report(BucketProgressEvent{
		Phase:           phase,
		MessageFallback: fallback,
		Percent:         pct,
	})
}

// Message returns the English fallback for the event.
func (e BucketProgressEvent) Message() string {
	if e.MessageKey != "" {
		return message.FormatEN(e.MessageKey, e.MessageArgs)
	}
	return e.MessageFallback
}
