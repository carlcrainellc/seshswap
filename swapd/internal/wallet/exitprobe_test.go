package wallet

import "context"

// No real network in tests: the probe "gets through" from a new IP.
func init() {
	exitProbe = func(context.Context) (string, error) { return "203.0.113.9", nil }
	exitIPBefore = func(context.Context) string { return "" }
}
