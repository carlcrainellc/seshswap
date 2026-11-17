package sysdns

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// HelperArg is the argv[1] that turns the wallet binary into the DNS helper:
//
//	<wallet> __exit-dns set LINK IP | restore | status
//
// It is meant to run as root through `sudo -n`. It only ever changes DNS for
// the user who ran sudo (SUDO_UID), and takes no paths from the caller.
const HelperArg = "__exit-dns"

// Result is the helper's answer on stdout.
type Result struct {
	OK       bool   `json:"ok"`
	Method   string `json:"method,omitempty"`
	Restored bool   `json:"restored,omitempty"`
	Error    string `json:"error,omitempty"`
}

func callerUID() int {
	if os.Geteuid() == 0 {
		if v, err := strconv.Atoi(os.Getenv("SUDO_UID")); err == nil && v > 0 {
			return v
		}
	}
	return os.Getuid()
}

// RunHelper runs one helper command and returns the exit code.
func RunHelper(args []string) int {
	res := do(&Switcher{}, callerUID(), args)
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	if !res.OK {
		return 1
	}
	return 0
}

func do(w *Switcher, uid int, args []string) Result {
	if os.Geteuid() != 0 && w.Dir == "" {
		return Result{Error: "needs root"}
	}
	switch {
	case len(args) == 3 && args[0] == "set":
		st, err := w.Set(uid, args[1], args[2])
		if err != nil {
			return Result{Error: err.Error()}
		}
		return Result{OK: true, Method: st.Method}
	case len(args) == 1 && args[0] == "restore":
		ok, err := w.Restore(uid)
		if err != nil {
			return Result{Error: err.Error()}
		}
		return Result{OK: true, Restored: ok}
	case len(args) == 1 && args[0] == "status":
		st, err := w.Status(uid)
		if err != nil {
			return Result{Error: err.Error()}
		}
		if st == nil {
			return Result{OK: true}
		}
		return Result{OK: true, Method: st.Method}
	}
	return Result{Error: "usage: " + HelperArg + " set LINK IP | restore | status"}
}

// Client asks the helper for changes. As root it does them in this process;
// otherwise it runs Exe (the wallet binary) under `sudo -n`, which needs a
// sudoers rule for that one command (see docs/EXITS.md).
type Client struct {
	Exe string
	// Exec is for tests: runs the helper with args and returns its stdout.
	Exec func(args ...string) ([]byte, error)
}

func (c *Client) call(args ...string) (Result, error) {
	if c.Exec == nil && os.Geteuid() == 0 {
		r := do(&Switcher{}, 0, args)
		if !r.OK {
			return r, errors.New(r.Error)
		}
		return r, nil
	}
	var out []byte
	var err error
	if c.Exec != nil {
		out, err = c.Exec(args...)
	} else {
		exe := c.Exe
		if exe == "" {
			if exe, err = os.Executable(); err != nil {
				return Result{}, err
			}
		}
		cmd := exec.Command("sudo", append([]string{"-n", exe, HelperArg}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err = cmd.Output()
		if err != nil && len(out) == 0 {
			msg := strings.TrimSpace(stderr.String())
			if strings.Contains(msg, "password") {
				msg = "sudo needs a password; add the sudoers rule from docs/EXITS.md"
			}
			return Result{}, fmt.Errorf("can't change system DNS: %s", msg)
		}
	}
	var r Result
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &r); jerr != nil {
		return r, fmt.Errorf("DNS helper: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if !r.OK {
		return r, errors.New(r.Error)
	}
	return r, nil
}

// Set points system DNS at server through link; returns the method used.
func (c *Client) Set(link, server string) (string, error) {
	r, err := c.call("set", link, server)
	return r.Method, err
}

// Restore puts system DNS back if it was changed; reports whether it was.
func (c *Client) Restore() (bool, error) {
	r, err := c.call("restore")
	return r.Restored, err
}
