package main

// Reading what Rusty says about exits: `rusty-ctl preflight --json` before a
// map (is another VPN connected?) and exit-guard.json, which Rusty's service
// writes when it unmaps an exit that didn't come up or stopped answering.

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// parsePreflight reads `rusty-ctl preflight --json`. It returns the message
// to show when mapping would not work ("" when it would) and any warnings.
func parsePreflight(out []byte) (string, []string, error) {
	var v struct {
		OK       *bool    `json:"ok"`
		Message  string   `json:"message"`
		Warnings []string `json:"warnings"`
	}
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	if err := json.Unmarshal([]byte(line), &v); err != nil || v.OK == nil {
		return "", nil, errors.New("odd reply from rusty-ctl preflight: " + truncate(line, 120))
	}
	if *v.OK {
		return "", v.Warnings, nil
	}
	if v.Message == "" {
		v.Message = "Another VPN is still holding your connection. Disconnect it or turn off its kill switch, then map again."
	}
	return v.Message, v.Warnings, nil
}

// parseGuardFile reads exit-guard.json.
func parseGuardFile(b []byte) (string, time.Time) {
	var v struct {
		Time    int64  `json:"time"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &v) != nil || v.Time == 0 {
		return "", time.Time{}
	}
	return v.Message, time.Unix(v.Time, 0)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
