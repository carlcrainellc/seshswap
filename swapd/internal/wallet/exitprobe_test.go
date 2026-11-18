package wallet

import "context"

// Unit tests have no internet: treat the exit traffic check as passing unless
// a test sets its own.
func init() { exitProbe = func(context.Context) error { return nil } }
