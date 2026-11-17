package exits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// RouterRPC talks to one Session Router's RPC for exit mapping. It works
// with the bundled router (ipc socket in its private temp dir) and with one
// that is already running on this computer (its [api] bind address). It only
// maps and unmaps exits; it never stops or reconfigures the router.
type RouterRPC struct {
	Endpoint string
	// Call is for tests; nil = OMQCall.
	Call func(ctx context.Context, endpoint, method string, payload []byte) ([]byte, error)
}

// RouteStatus is what map_exit and list_exits say about this computer's
// routes for the mapped exits. Known is false on builds that don't report it.
type RouteStatus struct {
	Known  bool
	Up     bool
	Reason string // why the routes aren't up, when the router says
}

func (st *RouteStatus) parse(res json.RawMessage) {
	var v struct {
		Up     *bool  `json:"routes_up"`
		Reason string `json:"routes_reason"`
	}
	if json.Unmarshal(res, &v) == nil && v.Up != nil {
		st.Known, st.Up, st.Reason = true, *v.Up, v.Reason
	}
}

// ExitList is the list_exits reply.
type ExitList struct {
	Routes RouteStatus
	Exits  []MappedExit
}

// MappedExit is one entry of list_exits.
type MappedExit struct {
	Address      string   `json:"address"`
	ExitCapable  bool     `json:"exit_capable"`
	SessionLive  bool     `json:"session_live"`
	Ranges       []string `json:"ranges"`
	LastActivity int64    `json:"last_activity"`
}

func (r *RouterRPC) call(ctx context.Context, method string, req any) (json.RawMessage, error) {
	if r == nil || r.Endpoint == "" {
		return nil, errors.New("no Session Router RPC address")
	}
	var payload []byte
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	f := r.Call
	if f == nil {
		f = OMQCall
	}
	body, err := f(ctx, r.Endpoint, "llarp."+method, payload)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if len(body) == 0 || string(body) == "null" {
		return nil, &UnknownCommandError{Method: method}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("odd reply from Session Router: %s", truncate(string(body), 200))
	}
	if out.Error != nil {
		return nil, fmt.Errorf("Session Router: %v", out.Error)
	}
	return out.Result, nil
}

// Version returns the router's version string (e.g. session-router-1.1.1-…).
func (r *RouterRPC) Version(ctx context.Context) (string, error) {
	res, err := r.call(ctx, "version", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		Full string `json:"version_full"`
	}
	_ = json.Unmarshal(res, &v)
	return v.Full, nil
}

// SupportsExits reports whether the router has the exit RPCs (our patched
// build); a stock build answers list_exits with nothing.
func (r *RouterRPC) SupportsExits(ctx context.Context) error {
	_, err := r.ListExits(ctx)
	return err
}

// MapExit sends all of this router's tunnel traffic (full tunnel) through exit.
func (r *RouterRPC) MapExit(ctx context.Context, exit string) (RouteStatus, error) {
	return r.mapExit(ctx, map[string]any{"address": exit, "full_tunnel": true, "ip_ranges": []string{}})
}

// MapRanges sends only the given IP ranges through exit.
func (r *RouterRPC) MapRanges(ctx context.Context, exit string, ranges []string) (RouteStatus, error) {
	return r.mapExit(ctx, map[string]any{"address": exit, "full_tunnel": false, "ip_ranges": ranges})
}

// mapExit: newer builds answer with the route status, older ones with a
// plain message.
func (r *RouterRPC) mapExit(ctx context.Context, req map[string]any) (RouteStatus, error) {
	var st RouteStatus
	res, err := r.call(ctx, "map_exit", req)
	if err == nil {
		st.parse(res)
	}
	return st, err
}

// UnmapExit stops using exit. Safe to call when it isn't mapped.
func (r *RouterRPC) UnmapExit(ctx context.Context, exit string) error {
	_, err := r.call(ctx, "unmap_exit", map[string]any{"address": exit})
	return err
}

// ListExits returns the mapped exits and the route status. Older builds
// answer with just the list.
func (r *RouterRPC) ListExits(ctx context.Context) (ExitList, error) {
	var out ExitList
	res, err := r.call(ctx, "list_exits", nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(res, &out.Exits); err == nil {
		return out, nil
	}
	var v struct {
		Exits []MappedExit `json:"exits"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return out, fmt.Errorf("odd list_exits reply: %s", truncate(string(res), 200))
	}
	out.Exits = v.Exits
	out.Routes.parse(res)
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
