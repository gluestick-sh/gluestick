package install

import (
	"fmt"
	"strings"
)

func phaseError(cause string, err error, hint string) error {
	if err == nil {
		return nil
	}
	if strings.TrimSpace(hint) == "" {
		return fmt.Errorf("%s: %w", cause, err)
	}
	return fmt.Errorf("%s: %w\n\n%s", cause, err, hint)
}

func downloadPhaseError(urlCount int, err error) error {
	hint := "Check your network connection or configure a GitHub mirror, then retry"
	if isHTTPNotFound(err) {
		hint = "The download URL does not exist. If you pinned @version, that release may be source-only or missing this architecture."
	}
	return phaseError(
		fmt.Sprintf("download failed after trying %d URL(s)", urlCount),
		err,
		hint,
	)
}

func isHTTPNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "404 Not Found")
}

func extractPhaseError(err error) error {
	return phaseError("extraction failed", err, "Run glue install 7zip to install 7-Zip, or reinstall the package")
}

func permissionPhaseError(cause string, err error) error {
	return phaseError(cause, err, "Check ~/.glue directory permissions or run as administrator")
}
