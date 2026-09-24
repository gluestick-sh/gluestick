// Package envset manages persistent user environment variables for the current
// user (the Scoop env_set / env_add_path manifest directives). On Windows these
// are stored under the HKCU\Environment registry key.
package envset

// ApplyUser sets persistent user environment variables (Scoop env_set).
func ApplyUser(vars map[string]string) error {
	return applyUser(vars)
}

// RemoveUser deletes persistent user environment variables by name.
func RemoveUser(names []string) error {
	return removeUser(names)
}
