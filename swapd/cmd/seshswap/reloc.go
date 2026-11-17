package main

// The bundled Session Router carries its own glibc and libraries so it runs on
// Ubuntu 22.04 as well as 24.04. Its ELF interpreter and RPATH must be
// absolute paths: after `setcap` the loader ignores LD_LIBRARY_PATH and
// $ORIGIN. scripts/package-linux.sh leaves fixed-size slots for both strings
// and records their file offsets in session-router/RELOC; this fills them in
// for wherever the bundle was unpacked. Writing the file clears its file
// capabilities, so this only writes when the paths are wrong (first run, or
// after the folder moved).

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type slot struct {
	name string
	off  int64
	size int
}

func readReloc(path string) ([]slot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []slot
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) != 3 || strings.HasPrefix(fl[0], "#") {
			continue
		}
		off, err1 := strconv.ParseInt(fl[1], 10, 64)
		size, err2 := strconv.Atoi(fl[2])
		if err1 != nil || err2 != nil || off <= 0 || size <= 0 {
			return nil, fmt.Errorf("bad line in %s: %q", path, sc.Text())
		}
		out = append(out, slot{fl[0], off, size})
	}
	return out, sc.Err()
}

// relocRouter makes the router's interpreter and library path point into
// srDir. It returns changed=true when it had to rewrite the binary.
func relocRouter(srDir string) (changed bool, err error) {
	slots, err := readReloc(filepath.Join(srDir, "RELOC"))
	if os.IsNotExist(err) {
		return false, nil // a router fetched by fetch-session-router.sh: already absolute
	}
	if err != nil {
		return false, err
	}
	bin := filepath.Join(srDir, "bin", "session-router")
	lib := filepath.Join(srDir, "lib")
	want := map[string]string{
		"interp": filepath.Join(lib, "ld-linux-x86-64.so.2"),
		"rpath":  lib,
	}
	f, err := os.OpenFile(bin, os.O_RDWR, 0)
	if err != nil {
		// Read-only install: fine if it is already right.
		f, err = os.Open(bin)
		if err != nil {
			return false, err
		}
	}
	defer f.Close()
	for _, s := range slots {
		w, ok := want[s.name]
		if !ok {
			return false, fmt.Errorf("unknown RELOC slot %q", s.name)
		}
		if len(w)+1 > s.size {
			return changed, fmt.Errorf("the SeshSwap folder path is too long (%s). Move the folder somewhere shorter, like ~/seshswap", srDir)
		}
		cur := make([]byte, s.size)
		if _, err := f.ReadAt(cur, s.off); err != nil {
			return changed, err
		}
		buf := make([]byte, s.size)
		copy(buf, w)
		if bytes.Equal(cur, buf) {
			continue
		}
		if _, err := f.WriteAt(buf, s.off); err != nil {
			return changed, fmt.Errorf("can't update %s (is the folder read-only?): %w", bin, err)
		}
		changed = true
	}
	return changed, nil
}
