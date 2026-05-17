package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/axgrid/axup/internal/protocol"
)

// downloadHTTPClient is shared across invocations so a long-lived agent
// process (currently single-shot, but the wire is ready) reuses TCP
// connections via the default Transport.
var downloadHTTPClient = &http.Client{
	Timeout: 10 * time.Minute,
}

// runDownloadTask fetches Plan.DownloadURL into Plan.DstPath. Idempotency
// is driven by the optional Sha256: when present, we compare the on-disk
// file's digest BEFORE downloading and skip on match. Without Sha256, we
// only check existence — a present file is assumed correct (the doc
// recommends pinning sha256 for any non-trivial use).
//
// The download writes to <dst>.download.tmp first and renames on success
// so a partial fetch never appears at dst. Mode defaults to 0644.
//
// Custom headers (Authorization, etc) come from Plan.DownloadHeaders.
// Errors surface the HTTP status text so private-registry 401/403 are
// obvious.
func runDownloadTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.DownloadURL == "" || t.DstPath == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "download: url and dst are required"}
	}
	mode, err := parseModeOr(t.Mode, 0o644)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "bad mode: " + err.Error()}
	}

	// Pre-check: if dst exists and (a) we have an expected sha that matches,
	// or (b) we have no expected sha at all, skip the network round-trip.
	st, statErr := os.Stat(t.DstPath)
	exists := statErr == nil
	if exists && !st.Mode().IsRegular() {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "exists and is not a regular file (refusing to overwrite)"}
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "stat: " + statErr.Error()}
	}
	if exists {
		if t.Sha256 != "" {
			cur, err := fileSha256(t.DstPath)
			if err != nil {
				return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "checksum: " + err.Error()}
			}
			if cur == t.Sha256 {
				// Mode might still drift — reconcile that separately.
				if st.Mode().Perm() != mode {
					if ctx.dryRun {
						return protocol.Event{Status: protocol.StatusWouldChange, Path: t.DstPath, Message: fmt.Sprintf("would chmod %04o → %04o (content already in sync)", st.Mode().Perm(), mode)}
					}
					if err := os.Chmod(t.DstPath, mode); err != nil {
						return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chmod: " + err.Error()}
					}
					ctx.changed[t.DstPath] = true
					return protocol.Event{Status: protocol.StatusChanged, Path: t.DstPath, Message: "reconciled mode"}
				}
				return protocol.Event{Status: protocol.StatusSkipped, Path: t.DstPath, Message: "sha matches"}
			}
			// fall through to re-download
		} else {
			// No expected sha — trust existing file.
			if st.Mode().Perm() != mode {
				if ctx.dryRun {
					return protocol.Event{Status: protocol.StatusWouldChange, Path: t.DstPath, Message: fmt.Sprintf("would chmod %04o → %04o (no sha pinned, treating file as current)", st.Mode().Perm(), mode)}
				}
				if err := os.Chmod(t.DstPath, mode); err != nil {
					return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chmod: " + err.Error()}
				}
				ctx.changed[t.DstPath] = true
				return protocol.Event{Status: protocol.StatusChanged, Path: t.DstPath, Message: "reconciled mode (no sha pinned)"}
			}
			return protocol.Event{Status: protocol.StatusSkipped, Path: t.DstPath, Message: "exists (no sha pinned, treating as current)"}
		}
	}

	if ctx.dryRun {
		ctx.changed[t.DstPath] = true
		why := "would download (file absent)"
		if exists {
			why = "would re-download (sha mismatch)"
		}
		return protocol.Event{Status: protocol.StatusWouldChange, Path: t.DstPath, Message: why + " from " + t.DownloadURL}
	}

	// Actual fetch.
	req, err := http.NewRequest(http.MethodGet, t.DownloadURL, nil)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "new request: " + err.Error()}
	}
	for k, v := range t.DownloadHeaders {
		req.Header.Set(k, v)
	}
	resp, err := downloadHTTPClient.Do(req)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "http: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: fmt.Sprintf("http %s from %s", resp.Status, t.DownloadURL)}
	}

	if err := os.MkdirAll(filepath.Dir(t.DstPath), 0o755); err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "mkdir parent: " + err.Error()}
	}
	tmp := t.DstPath + ".download.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "create tmp: " + err.Error()}
	}
	hasher := sha256.New()
	wrote, err := io.Copy(io.MultiWriter(out, hasher), resp.Body)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "write: " + err.Error()}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "close tmp: " + err.Error()}
	}
	gotSha := hex.EncodeToString(hasher.Sum(nil))
	if t.Sha256 != "" && gotSha != t.Sha256 {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: fmt.Sprintf("sha mismatch: expected %s got %s", t.Sha256, gotSha)}
	}
	// Re-chmod the tmp file — open's mode is masked by umask. Belt + braces.
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chmod tmp: " + err.Error()}
	}
	if err := os.Rename(tmp, t.DstPath); err != nil {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "rename: " + err.Error()}
	}
	ctx.changed[t.DstPath] = true
	return protocol.Event{Status: protocol.StatusChanged, Path: t.DstPath, Message: fmt.Sprintf("downloaded %d bytes (sha %s)", wrote, gotSha[:12])}
}

// fileSha256 streams a file through sha256 — used to decide skip-vs-download
// against the optional Plan.Sha256.
func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
