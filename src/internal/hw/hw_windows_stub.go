//go:build !windows

package hw

func readWindowsMem() (total, avail uint64) {
	return 0, 0
}
