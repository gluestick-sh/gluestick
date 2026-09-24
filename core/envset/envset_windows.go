//go:build windows

package envset

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const userEnvKey = `Environment`

func applyUser(vars map[string]string) error {
	if len(vars) == 0 {
		return nil
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open user Environment key: %w", err)
	}
	defer key.Close()

	for name, value := range vars {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := key.SetStringValue(name, value); err != nil {
			return fmt.Errorf("set user env %s: %w", name, err)
		}
	}
	return nil
}

func removeUser(names []string) error {
	if len(names) == 0 {
		return nil
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open user Environment key: %w", err)
	}
	defer key.Close()

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := key.DeleteValue(name); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("remove user env %s: %w", name, err)
		}
	}
	return nil
}
