//go:build seshswap_setup

package main

import (
	"strings"
)

// serviceInstallDir is the folder of the service program in a Windows
// ImagePath / BinaryPathName, or "".
//
// Windows stores that field in two ways:
//
//	"C:\Path\rusty-service.exe" service --config C:\Path\rusty-service.json
//	C:\Path\rusty-service.exe service --config C:\Path\rusty-service.json
//
// The second form is what rusty-setup.exe / rusty-service writes today when
// the path has no spaces (rusty-windows; that installer is public and is not
// changed here). Taking filepath.Dir of the whole unquoted string would yield
// a mangled folder and hide my-client-address.txt on reinstall.
func serviceInstallDir(binaryPathName string) string {
	exe := serviceExe(binaryPathName)
	if exe == "" {
		return ""
	}
	i := strings.LastIndexAny(exe, `/\`)
	if i <= 0 {
		return exe
	}
	return exe[:i]
}

// serviceExe is the program path in a Windows service ImagePath.
func serviceExe(binaryPathName string) string {
	s := strings.TrimSpace(binaryPathName)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		end := strings.Index(s[1:], `"`)
		if end < 0 {
			return strings.TrimSpace(s[1:])
		}
		return s[1 : 1+end]
	}
	// Unquoted: the program is the path through ".exe" when present,
	// otherwise the first token.
	low := strings.ToLower(s)
	if i := strings.Index(low, ".exe"); i >= 0 {
		return s[:i+4]
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}
