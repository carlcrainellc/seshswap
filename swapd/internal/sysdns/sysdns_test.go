package sysdns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileSwitcher(t *testing.T) (*Switcher, string) {
	d := t.TempDir()
	return &Switcher{Dir: filepath.Join(d, "state"), ResolvConf: filepath.Join(d, "resolv.conf"),
		Resolved: func(string) bool { return false }}, d
}

func TestFileSetRestore(t *testing.T) {
	w, _ := fileSwitcher(t)
	orig := "nameserver 192.168.1.1\nsearch lan\n"
	os.WriteFile(w.ResolvConf, []byte(orig), 0o644)
	st, err := w.Set(1000, "swr0a1b2c", "10.195.0.53")
	if err != nil || st.Method != "file" {
		t.Fatal(st, err)
	}
	b, _ := os.ReadFile(w.ResolvConf)
	if !strings.Contains(string(b), "nameserver 10.195.0.53") || strings.Contains(string(b), "192.168.1.1") {
		t.Fatalf("written %q", b)
	}
	// A fresh process (after a crash) finds the change and undoes it.
	w2 := &Switcher{Dir: w.Dir, ResolvConf: w.ResolvConf}
	if s, _ := w2.Status(1000); s == nil || s.Server != "10.195.0.53" {
		t.Fatalf("status %+v", s)
	}
	if ok, err := w2.Restore(1000); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if b, _ := os.ReadFile(w.ResolvConf); string(b) != orig {
		t.Fatalf("restored %q", b)
	}
	if ok, err := w2.Restore(1000); ok || err != nil {
		t.Fatal("second restore should do nothing", ok, err)
	}
}

func TestFileInPlaceKeepsInode(t *testing.T) {
	w, _ := fileSwitcher(t)
	os.WriteFile(w.ResolvConf, []byte("nameserver 1.1.1.1\n"), 0o644)
	before, _ := os.Stat(w.ResolvConf)
	w.Set(1, "tun0", "10.195.0.53")
	after, _ := os.Stat(w.ResolvConf)
	if !os.SameFile(before, after) {
		t.Fatal("resolv.conf was replaced, not rewritten (breaks bind mounts)")
	}
}

func TestFileSymlink(t *testing.T) {
	w, d := fileSwitcher(t)
	target := filepath.Join(d, "stub-resolv.conf")
	os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0o644)
	os.Symlink(target, w.ResolvConf)
	if _, err := w.Set(1, "tun0", "10.195.0.53"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(w.ResolvConf); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("still a symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "nameserver 127.0.0.53\n" {
		t.Fatal("the symlink target was changed")
	}
	w.Restore(1)
	if l, err := os.Readlink(w.ResolvConf); err != nil || l != target {
		t.Fatalf("symlink not restored: %q %v", l, err)
	}
}

func TestFileMissingAndForeignRewrite(t *testing.T) {
	w, _ := fileSwitcher(t)
	w.Set(1, "tun0", "10.195.0.53")
	w.Restore(1)
	if _, err := os.Stat(w.ResolvConf); !os.IsNotExist(err) {
		t.Fatal("a resolv.conf that didn't exist should be removed again")
	}
	os.WriteFile(w.ResolvConf, []byte("nameserver 9.9.9.9\n"), 0o644)
	w.Set(1, "tun0", "10.195.0.53")
	os.WriteFile(w.ResolvConf, []byte("nameserver 8.8.8.8\n# from dhcp\n"), 0o644)
	if ok, err := w.Restore(1); ok || err != nil {
		t.Fatal(ok, err)
	}
	if b, _ := os.ReadFile(w.ResolvConf); !strings.Contains(string(b), "8.8.8.8") {
		t.Fatal("restore clobbered a newer resolv.conf")
	}
	if s, _ := w.Status(1); s != nil {
		t.Fatal("state left behind")
	}
}

func TestResolved(t *testing.T) {
	w, _ := fileSwitcher(t)
	var cmds []string
	w.Resolved = func(string) bool { return true }
	w.Run = func(name string, args ...string) error {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		return nil
	}
	if st, err := w.Set(7, "lo", "10.195.0.53"); err != nil || st.Method != "resolved" {
		t.Fatal(st, err)
	}
	want := "resolvectl dns lo 10.195.0.53|resolvectl domain lo ~.|resolvectl default-route lo yes|resolvectl flush-caches"
	if strings.Join(cmds, "|") != want {
		t.Fatalf("cmds %v", cmds)
	}
	cmds = nil
	if ok, _ := w.Restore(7); !ok || cmds[0] != "resolvectl revert lo" {
		t.Fatalf("restore %v", cmds)
	}
	// link already gone: nothing to revert, state still cleared
	w.Set(7, "nosuchlink0", "10.195.0.53")
	cmds = nil
	if ok, err := w.Restore(7); !ok || err != nil || len(cmds) != 0 {
		t.Fatalf("gone link %v %v %v", ok, err, cmds)
	}
}

func TestBadInput(t *testing.T) {
	w, _ := fileSwitcher(t)
	if _, err := w.Set(1, "tun0", "10.195.0.53; rm -rf /"); err == nil {
		t.Fatal("bad server accepted")
	}
	if _, err := w.Set(1, "../../x", "10.195.0.53"); err == nil {
		t.Fatal("bad link accepted")
	}
	if r := do(w, 1, []string{"set", "tun0"}); r.OK {
		t.Fatal("usage")
	}
	if r := do(w, 1, []string{"set", "tun0", "10.195.0.53"}); !r.OK || r.Method != "file" {
		t.Fatal(r)
	}
	if r := do(w, 1, []string{"restore"}); !r.OK || !r.Restored {
		t.Fatal(r)
	}
}

func TestClientParsesHelper(t *testing.T) {
	c := &Client{Exec: func(args ...string) ([]byte, error) {
		if args[0] == "set" {
			return []byte(`{"ok":true,"method":"file"}`), nil
		}
		return []byte(`{"ok":false,"error":"boom"}`), os.ErrPermission
	}}
	if m, err := c.Set("tun0", "10.195.0.53"); m != "file" || err != nil {
		t.Fatal(m, err)
	}
	if _, err := c.Restore(); err == nil || err.Error() != "boom" {
		t.Fatal(err)
	}
}
