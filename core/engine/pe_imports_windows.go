//go:build windows

package engine

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	peMagicPE32Plus      = 0x20b
	peImportDirOffPE32Plus = 120
	peSectionHeaderSize  = 40
)

// missingSameDirPeImports returns bundled import DLL names missing beside the executable.
// Windows system/redist DLLs resolved from System32 are ignored.
func missingSameDirPeImports(exePath string) ([]string, error) {
	imports, err := peImportDLLs(exePath)
	if err != nil || len(imports) == 0 {
		return nil, err
	}
	exeDir := filepath.Dir(exePath)
	var missing []string
	for _, dll := range imports {
		dll = strings.TrimSpace(dll)
		if dll == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(exeDir, dll)); err == nil {
			continue
		}
		if dllAvailableOnSystem(dll) {
			continue
		}
		missing = append(missing, dll)
	}
	return missing, nil
}

func peImportDLLs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 64 || data[0] != 'M' || data[1] != 'Z' {
		return nil, fmt.Errorf("not a PE file")
	}
	peOffset := int(binary.LittleEndian.Uint32(data[0x3C:]))
	if peOffset <= 0 || peOffset+24 > len(data) {
		return nil, fmt.Errorf("invalid PE offset")
	}
	if string(data[peOffset:peOffset+4]) != "PE\x00\x00" {
		return nil, fmt.Errorf("missing PE signature")
	}

	optOff := peOffset + 24
	if optOff+2 > len(data) {
		return nil, fmt.Errorf("truncated optional header")
	}
	if binary.LittleEndian.Uint16(data[optOff:]) != peMagicPE32Plus {
		return nil, nil
	}
	if optOff+peImportDirOffPE32Plus+8 > len(data) {
		return nil, fmt.Errorf("truncated import directory")
	}

	importRVA := binary.LittleEndian.Uint32(data[optOff+peImportDirOffPE32Plus:])
	if importRVA == 0 {
		return nil, nil
	}

	numSections := int(binary.LittleEndian.Uint16(data[peOffset+6:]))
	sectionOff := optOff + int(binary.LittleEndian.Uint16(data[peOffset+20:]))
	if sectionOff+numSections*peSectionHeaderSize > len(data) {
		return nil, fmt.Errorf("truncated section table")
	}

	rvaToOff := func(rva uint32) (int, bool) {
		for i := 0; i < numSections; i++ {
			base := sectionOff + i*peSectionHeaderSize
			virtualSize := binary.LittleEndian.Uint32(data[base+8:])
			virtualAddr := binary.LittleEndian.Uint32(data[base+12:])
			rawSize := binary.LittleEndian.Uint32(data[base+16:])
			rawPtr := binary.LittleEndian.Uint32(data[base+20:])
			size := virtualSize
			if rawSize < size {
				size = rawSize
			}
			if rva >= virtualAddr && rva < virtualAddr+size {
				return int(rawPtr + (rva - virtualAddr)), true
			}
		}
		return 0, false
	}

	importOff, ok := rvaToOff(importRVA)
	if !ok {
		return nil, fmt.Errorf("import directory RVA not mapped")
	}

	var out []string
	for importOff+20 <= len(data) {
		origThunk := binary.LittleEndian.Uint32(data[importOff:])
		nameRVA := binary.LittleEndian.Uint32(data[importOff+12:])
		if origThunk == 0 && nameRVA == 0 {
			break
		}
		if nameRVA != 0 {
			nameOff, ok := rvaToOff(nameRVA)
			if ok {
				if dll := readCString(data, nameOff); dll != "" {
					out = append(out, dll)
				}
			}
		}
		importOff += 20
	}
	return out, nil
}

func readCString(data []byte, off int) string {
	if off < 0 || off >= len(data) {
		return ""
	}
	end := off
	for end < len(data) && data[end] != 0 {
		end++
	}
	return string(data[off:end])
}

func isSystemImportDLL(name string) bool {
	switch strings.ToLower(name) {
	case "kernel32.dll", "user32.dll", "msvcrt.dll", "advapi32.dll", "shell32.dll",
		"ole32.dll", "oleaut32.dll", "ws2_32.dll", "ntdll.dll", "comctl32.dll",
		"gdi32.dll", "comdlg32.dll", "shlwapi.dll", "ucrtbase.dll", "sechost.dll",
		"imm32.dll", "winmm.dll", "version.dll", "setupapi.dll",
		"netapi32.dll", "iphlpapi.dll", "userenv.dll", "bcrypt.dll", "crypt32.dll",
		"psapi.dll", "powrprof.dll", "wldap32.dll", "normaliz.dll", "winhttp.dll",
		"wininet.dll", "dbghelp.dll", "rpcrt4.dll", "shcore.dll", "propsys.dll":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "api-ms-win-")
}

func dllAvailableOnSystem(dll string) bool {
	if isSystemImportDLL(dll) {
		return true
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	for _, dir := range []string{
		filepath.Join(root, "System32"),
		filepath.Join(root, "SysWOW64"),
	} {
		if _, err := os.Stat(filepath.Join(dir, dll)); err == nil {
			return true
		}
	}
	return false
}
