package install

var (
	colorReset = "\033[0m"
	colorGreen = "\033[32m"
	colorRed   = "\033[31m"
)

// SetColorEnabled toggles ANSI color codes in verbose progress marks.
func SetColorEnabled(enabled bool) {
	if enabled {
		colorReset = "\033[0m"
		colorGreen = "\033[32m"
		colorRed = "\033[31m"
	} else {
		colorReset = ""
		colorGreen = ""
		colorRed = ""
	}
}

// SuccessMark returns a checkmark for verbose CLI output.
func SuccessMark() string {
	return colorGreen + "\u2713" + colorReset
}

// FailedMark returns a cross mark for verbose CLI output.
func FailedMark() string {
	return colorRed + "\u2717" + colorReset
}

func successMark() string { return SuccessMark() }

func failedMark() string { return FailedMark() }
