//go:build windows

package procutil

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func findProcessesRunningFrom(dirs []string) ([]RunningProcess, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	var out []RunningProcess
	seen := make(map[uint32]struct{})

	var pe32 windows.ProcessEntry32
	pe32.Size = uint32(unsafe.Sizeof(pe32))

	for err := windows.Process32First(snapshot, &pe32); err == nil; err = windows.Process32Next(snapshot, &pe32) {
		pid := pe32.ProcessID
		if pid == 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}

		exePath, err := processImagePath(pid)
		if err != nil || exePath == "" {
			continue
		}

		matched := false
		for _, dir := range dirs {
			if pathUnderDir(exePath, dir) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		seen[pid] = struct{}{}
		out = append(out, runningProcessFromEntry(pid, pe32.ExeFile[:], exePath))
	}
	return out, nil
}

func findProcessesWithModulesFrom(dirs []string) ([]RunningProcess, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	var out []RunningProcess
	seen := make(map[uint32]struct{})

	var pe32 windows.ProcessEntry32
	pe32.Size = uint32(unsafe.Sizeof(pe32))

	for err := windows.Process32First(snapshot, &pe32); err == nil; err = windows.Process32Next(snapshot, &pe32) {
		pid := pe32.ProcessID
		if pid == 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}

		modPath, ok := processModuleUnderDirs(pid, dirs)
		if !ok {
			continue
		}

		seen[pid] = struct{}{}
		p := runningProcessFromEntry(pid, pe32.ExeFile[:], modPath)
		out = append(out, p)
	}
	return out, nil
}

func processModuleUnderDirs(pid uint32, dirs []string) (string, bool) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPMODULE|windows.TH32CS_SNAPMODULE32, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(snapshot)

	var me32 windows.ModuleEntry32
	me32.Size = uint32(unsafe.Sizeof(me32))
	for err := windows.Module32First(snapshot, &me32); err == nil; err = windows.Module32Next(snapshot, &me32) {
		modPath := strings.TrimSpace(windows.UTF16ToString(me32.ExePath[:]))
		if modPath == "" {
			continue
		}
		for _, dir := range dirs {
			if pathUnderDir(modPath, dir) {
				return modPath, true
			}
		}
	}
	return "", false
}

func runningProcessFromEntry(pid uint32, exeFile []uint16, path string) RunningProcess {
	name := strings.TrimSpace(windows.UTF16ToString(exeFile))
	if name == "" {
		name = filepath.Base(path)
	}
	return RunningProcess{
		PID:  int(pid),
		Name: name,
		Path: path,
	}
}

func processImagePath(pid uint32) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	buf := make([]uint16, windows.MAX_PATH)
	for {
		size := uint32(len(buf))
		err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size)
		if err == nil {
			return windows.UTF16ToString(buf[:size]), nil
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER {
			return "", err
		}
		buf = make([]uint16, len(buf)*2)
	}
}
