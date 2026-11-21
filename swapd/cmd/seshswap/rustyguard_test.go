package main

import (
	"testing"
	"time"
)

func TestParsePreflight(t *testing.T) {
	const nord = "Another VPN is still holding your connection (NordVPN). Disconnect it or turn off its kill switch, then map again."
	msg, _, err := parsePreflight([]byte(`{"ok":false,"code":"other_vpn","message":"` + nord + `","vpn":{"name":"NordVPN"},"warnings":[]}` + "\r\n"))
	if err != nil || msg != nord {
		t.Fatalf("%q %v", msg, err)
	}
	msg, w, err := parsePreflight([]byte(`{"ok":true,"warnings":["Proton VPN still has a default route (ProtonVPN)."],"last_rollback":{"why":"exit_lost"}}`))
	if err != nil || msg != "" || len(w) != 1 {
		t.Fatalf("%q %v %v", msg, w, err)
	}
	// An older rusty-ctl without preflight prints usage, not JSON.
	if _, _, err := parsePreflight([]byte("error: unrecognized subcommand 'preflight'")); err == nil {
		t.Fatal("took usage text as a pass")
	}
}

func TestParseGuardFile(t *testing.T) {
	msg, at := parseGuardFile([]byte(`{"time":1795300000,"addresses":["a.sesh"],"why":"exit_not_answering","message":"Exit a isn't answering"}`))
	if msg != "Exit a isn't answering" || !at.Equal(time.Unix(1795300000, 0)) {
		t.Fatalf("%q %v", msg, at)
	}
	if msg, _ := parseGuardFile([]byte(`nope`)); msg != "" {
		t.Fatal(msg)
	}
}
