//go:build !windows

package main

import "fmt"

func openBrowser(url string) error {
	return fmt.Errorf("open browser: not supported on this platform")
}
