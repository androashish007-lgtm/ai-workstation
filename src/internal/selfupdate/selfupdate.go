// Package selfupdate checks whether a newer version of this app has been
// published, and (via the platform update scripts, not this package) lets
// the user fetch it. There's no CI/release pipeline for this project yet —
// binaries are committed directly to bin/ on the default branch rather than
// published as GitHub Releases — so "latest" means "whatever VERSION says
// on that branch right now", fetched as a plain raw file rather than through
// the GitHub Releases API.
package selfupdate

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// CurrentVersion is bumped by hand alongside the repo's own VERSION file
// (kept as two copies deliberately: this one ships inside the binary so a
// running instance always knows what it is, even offline; the file is
// what a fresh checkout/update compares against).
const CurrentVersion = "0.1.0"

const versionURL = "https://raw.githubusercontent.com/androashish007-lgtm/ai-workstation/master/VERSION"

// Status is the result of one check — never fetched automatically, only
// when the user explicitly asks (a "Check for updates" button), so this
// package makes no network call on its own initiative.
type Status struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Error           string `json:"error,omitempty"`
	RepoURL         string `json:"repo_url"`
}

// Check fetches the latest published VERSION and compares it to
// CurrentVersion. A fetch failure (offline, GitHub unreachable) is
// reported in Status.Error rather than returned as an error — this is a
// "nice to know" check, never something that should look like the app
// itself is broken when it's just that this machine has no network route
// to GitHub right now.
func Check() Status {
	st := Status{Current: CurrentVersion, RepoURL: "https://github.com/androashish007-lgtm/ai-workstation"}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(versionURL)
	if err != nil {
		st.Error = "could not reach GitHub: " + err.Error()
		return st
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		st.Error = "unexpected response fetching VERSION: " + resp.Status
		return st
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		st.Error = "reading VERSION: " + err.Error()
		return st
	}
	st.Latest = strings.TrimSpace(string(b))
	st.UpdateAvailable = st.Latest != "" && st.Latest != st.Current
	return st
}
