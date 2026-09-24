package main

import (
	"errors"
	"strings"
)

// errUsage marks CLI usage errors (unknown flag, wrong arity, unknown subcommand).
var errUsage = errors.New("usage error")

type usageError struct {
	err error
}

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return errUsage }

func wrapUsageError(err error) error {
	if err == nil {
		return nil
	}
	return usageError{err: err}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errUsage) || isCobraUsageError(err) {
		return 2
	}
	return 1
}

func isCobraUsageError(err error) bool {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unknown command"),
		strings.Contains(msg, "unknown flag"),
		strings.Contains(msg, "unknown shorthand"),
		strings.Contains(msg, "invalid argument"),
		strings.Contains(msg, "accepts "),
		strings.Contains(msg, "requires at least"),
		strings.Contains(msg, "requires only"),
		strings.Contains(msg, "flag needs an argument"):
		return true
	default:
		return false
	}
}
