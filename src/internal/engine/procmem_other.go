//go:build !windows && !linux && !darwin

package engine

// processRSSBytes has no implementation on this platform — callers already
// treat ok==false as "just skip the percentage," so this degrades to a
// plain elapsed-time heartbeat rather than failing anything.
func processRSSBytes(pid int) (uint64, bool) { return 0, false }
