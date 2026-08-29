// Package safego runs a function in its own goroutine with panic recovery.
// A panic in a normal `go func(){...}()` is fatal to the entire process —
// unlike an HTTP handler panic (caught per-request by the server's own
// recover middleware), nothing protects a background goroutine by default.
// For an app whose whole design promise is "self-heal, never crash," every
// long-running or fire-and-forget goroutine should go through this instead.
package safego

import "log"

// Go runs fn in a new goroutine; a panic inside it is logged instead of
// taking down the process.
func Go(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("recovered panic in background goroutine: %v", r)
			}
		}()
		fn()
	}()
}
