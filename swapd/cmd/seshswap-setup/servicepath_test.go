//go:build seshswap_setup

package main

import "testing"

func TestServiceInstallDir(t *testing.T) {
	cases := []struct {
		in, exe, dir string
	}{
		{
			in:  `C:\Rusty\rusty-service.exe service --config C:\Rusty\rusty-service.json`,
			exe: `C:\Rusty\rusty-service.exe`,
			dir: `C:\Rusty`,
		},
		{
			in:  `"C:\Rusty\rusty-service.exe" service --config C:\Rusty\rusty-service.json`,
			exe: `C:\Rusty\rusty-service.exe`,
			dir: `C:\Rusty`,
		},
		{
			in:  `"C:\Program Files\Rusty\rusty-service.exe" service --config "C:\Program Files\Rusty\rusty-service.json"`,
			exe: `C:\Program Files\Rusty\rusty-service.exe`,
			dir: `C:\Program Files\Rusty`,
		},
		{
			in:  `C:\Rusty\rusty-service.exe`,
			exe: `C:\Rusty\rusty-service.exe`,
			dir: `C:\Rusty`,
		},
		{
			in:  `  "C:\Rusty\rusty-service.exe" run  `,
			exe: `C:\Rusty\rusty-service.exe`,
			dir: `C:\Rusty`,
		},
		{
			in:  `C:\Tools\svc.exe /service`,
			exe: `C:\Tools\svc.exe`,
			dir: `C:\Tools`,
		},
		{in: "", exe: "", dir: ""},
	}
	for _, c := range cases {
		if got := serviceExe(c.in); got != c.exe {
			t.Errorf("serviceExe(%q)=%q, want %q", c.in, got, c.exe)
		}
		if got := serviceInstallDir(c.in); got != c.dir {
			t.Errorf("serviceInstallDir(%q)=%q, want %q", c.in, got, c.dir)
		}
	}
}
