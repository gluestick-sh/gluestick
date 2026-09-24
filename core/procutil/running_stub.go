//go:build !windows

package procutil

func findProcessesRunningFrom(dirs []string) ([]RunningProcess, error) {
	return nil, nil
}

func findProcessesWithModulesFrom(dirs []string) ([]RunningProcess, error) {
	return nil, nil
}
