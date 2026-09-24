package bucket

var (
	colorReset = "\033[0m"
	colorGreen = "\033[32m"
)

// SetColorEnabled toggles ANSI color codes in bucket CLI output.
func SetColorEnabled(enabled bool) {
	if enabled {
		colorReset = "\033[0m"
		colorGreen = "\033[32m"
	} else {
		colorReset = ""
		colorGreen = ""
	}
}