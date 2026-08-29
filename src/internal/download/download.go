// Package download implements a resumable, checksum-verified HTTP fetcher
// used for both engine binaries and model weights. Everything lands in a
// data/downloads/*.part file first and is only atomically renamed into place
// once fully written (and, when a checksum is known, verified) — so a killed
// process or unplugged drive never leaves a corrupt file where a model or
// engine binary is expected.
package download

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Progress is reported periodically during a download.
type Progress struct {
	DoneBytes  int64
	TotalBytes int64 // 0 if the server didn't report Content-Length
}

// Options configures one download.
type Options struct {
	URL string
	// Dest is the final path the file should exist at once complete.
	Dest string
	// ExpectedSHA256, if non-empty, is verified after download; mismatch
	// deletes the output and returns an error rather than leaving a file
	// that looks installed but isn't trustworthy.
	ExpectedSHA256 string
	OnProgress     func(Progress)
}

// Result is returned on success.
type Result struct {
	Bytes  int64
	SHA256 string // always computed, even if ExpectedSHA256 was empty (TOFU)
}

var client = &http.Client{Timeout: 0} // streaming downloads: no fixed deadline, rely on context/cancel upstream

func Fetch(opts Options) (Result, error) {
	if err := os.MkdirAll(filepath.Dir(opts.Dest), 0o755); err != nil {
		return Result{}, err
	}
	partPath := opts.Dest + ".part"

	var startAt int64
	if fi, err := os.Stat(partPath); err == nil {
		startAt = fi.Size()
	}

	req, err := http.NewRequest(http.MethodGet, opts.URL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", "aistation/1.0 (+offline portable AI workstation)")
	if startAt > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startAt))
	}

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	resumed := false
	switch resp.StatusCode {
	case http.StatusOK:
		flags |= os.O_TRUNC
		startAt = 0
	case http.StatusPartialContent:
		flags |= os.O_APPEND
		resumed = true
	default:
		return Result{}, fmt.Errorf("download failed: HTTP %d for %s", resp.StatusCode, opts.URL)
	}

	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return Result{}, err
	}

	total := resp.ContentLength
	if resumed && total > 0 {
		total += startAt
	}

	hasher := sha256.New()
	if resumed {
		// Re-hash what's already on disk so the final checksum covers the
		// whole file, not just the newly-fetched tail.
		existing, err := os.Open(partPath)
		if err == nil {
			io.Copy(hasher, io.LimitReader(existing, startAt))
			existing.Close()
		}
	}

	writer := io.MultiWriter(f, hasher)
	done := startAt
	buf := make([]byte, 256*1024)
	lastReport := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := writer.Write(buf[:n]); werr != nil {
				f.Close()
				return Result{}, werr
			}
			done += int64(n)
			if opts.OnProgress != nil && time.Since(lastReport) > 200*time.Millisecond {
				opts.OnProgress(Progress{DoneBytes: done, TotalBytes: total})
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return Result{}, rerr
		}
	}
	f.Close()
	if opts.OnProgress != nil {
		opts.OnProgress(Progress{DoneBytes: done, TotalBytes: total})
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if opts.ExpectedSHA256 != "" && sum != opts.ExpectedSHA256 {
		os.Remove(partPath)
		return Result{}, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", opts.URL, opts.ExpectedSHA256, sum)
	}
	if err := os.Rename(partPath, opts.Dest); err != nil {
		return Result{}, err
	}
	return Result{Bytes: done, SHA256: sum}, nil
}

// VerifyExisting re-hashes a file already on disk and compares it against a
// known checksum. Used on every startup to catch drive corruption or a
// half-copied file left over from moving the folder between machines.
func VerifyExisting(path, expectedSHA256 string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == expectedSHA256, nil
}
