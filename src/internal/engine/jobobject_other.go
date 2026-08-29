//go:build !windows

package engine

import "os"

// assignToJobObject is a Windows-specific mitigation (see jobobject_windows.go)
// for orphaned child processes surviving a hard kill of this app. Unix
// process trees have their own (different) tools for this — process
// groups, Pdeathsig on Linux — not yet wired up here; left as a no-op
// rather than a half-implementation with a mismatched calling convention.
func assignToJobObject(process *os.Process) {}
