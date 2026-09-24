package message

// ProgressPayload is a structured progress update for GUI i18n.
type ProgressPayload struct {
	MessageKey  string                 `json:"messageKey"`
	MessageArgs map[string]interface{} `json:"messageArgs,omitempty"`
}

// NewProgress builds a progress payload with optional args.
func NewProgress(key string, args map[string]interface{}) ProgressPayload {
	if args == nil {
		args = map[string]interface{}{}
	}
	return ProgressPayload{MessageKey: key, MessageArgs: args}
}

// Text returns the English fallback message.
func (p ProgressPayload) Text() string {
	return FormatEN(p.MessageKey, p.MessageArgs)
}
