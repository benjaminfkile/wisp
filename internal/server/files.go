package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// File ingress caps (see task 227). These are the pinned per-request bounds
// applied to the optional files: [{path, content_base64}] array on
// POST /contracts. They are validation errors, never silent clamps: a create
// that violates any of them is rejected with a 400.
const (
	// maxFilesPerCreate caps the number of files a single create may ship. A
	// caller with more files than this must fold them into a tarball inside
	// userdata rather than bloating the create call.
	maxFilesPerCreate = 16

	// maxTotalFileBytes caps the total decoded size of every file in a create
	// summed together. A create that exceeds it is rejected with a 400.
	maxTotalFileBytes = 1 * 1024 * 1024

	// maxFilePathLen bounds the length of any single file path so the create
	// call cannot smuggle unbounded strings into container-side paths.
	maxFilePathLen = 256
)

// defaultMaxFileReadBytes is the download cap applied to GET /contracts/{id}/files
// when the operator has not overridden it via WISP_MAX_FILE_READ_BYTES. The
// task pins the default at 16 MiB; the config layer parses the env var as a
// positive int and threads it into the broker via Daemon.SetMaxFileReadBytes.
const defaultMaxFileReadBytes int64 = 16 * 1024 * 1024

// fileEntry mirrors the {path, content_base64} pair the create call accepts.
// Wire-shape fields are snake_case to match the wisp API convention.
type fileEntry struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
}

// validateAndDecodeFiles applies the pinned caps and path shape rules to the
// files array from a create request, decoding each content_base64 into raw
// bytes ready to hand to the runtime. It returns a validation error (suitable
// for a 400) on any breach so the create is rejected up front; nil (with a
// nil slice) when the caller supplied no files at all.
//
// Every check is a REJECTION, never a silent clamp: the wire contract is
// pinned across repos, so admitting a truncated or reshaped file set would
// mislead an upstream that trusts the caps. Errors carry a client-facing
// message that names the offending path where useful, but never echoes any
// file content.
func validateAndDecodeFiles(files []fileEntry) ([]runtime.FileEntry, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > maxFilesPerCreate {
		return nil, fmt.Errorf("files has too many entries (max %d)", maxFilesPerCreate)
	}
	seen := make(map[string]struct{}, len(files))
	out := make([]runtime.FileEntry, 0, len(files))
	total := 0
	for _, f := range files {
		if err := validateFilePath(f.Path); err != nil {
			return nil, err
		}
		if _, dup := seen[f.Path]; dup {
			return nil, fmt.Errorf("files: duplicate path %q", f.Path)
		}
		seen[f.Path] = struct{}{}
		content, err := base64.StdEncoding.DecodeString(f.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("files: invalid base64 for path %q", f.Path)
		}
		total += len(content)
		if total > maxTotalFileBytes {
			return nil, fmt.Errorf("files exceed total size cap (max %d bytes)", maxTotalFileBytes)
		}
		out = append(out, runtime.FileEntry{Path: f.Path, Content: content})
	}
	return out, nil
}

// validateFilePath enforces the pinned path shape: non-empty, absolute
// unix-style (starts with /), no backslash, no ".." segment, at most
// maxFilePathLen characters, and no NUL byte. It is used for BOTH the create
// path (against every entry in files[]) and the download endpoint's ?path=
// query argument, so the two surfaces enforce identical rules.
func validateFilePath(p string) error {
	if p == "" {
		return errors.New("files: path must not be empty")
	}
	if len(p) > maxFilePathLen {
		return fmt.Errorf("files: path too long (max %d bytes)", maxFilePathLen)
	}
	if p[0] != '/' {
		return fmt.Errorf("files: path must be absolute (unix-style, starts with '/'): %q", p)
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("files: path must not contain '\\': %q", p)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("files: path must not contain NUL: %q", p)
	}
	// Reject any component equal to "..". Splitting on '/' catches both
	// leading, trailing, and middle traversal (e.g. "/foo/../bar", "/..", "/a/..").
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("files: path must not contain '..' segment: %q", p)
		}
	}
	return nil
}

// fileDownload handles GET /contracts/{id}/files?path=/abs/path. It streams a
// single regular file's raw bytes back as application/octet-stream. Auth
// mirrors GET /contracts/{id}: the app token OR the contract's own bearer
// token authorizes it (see authorizedForContract), so an agent that only
// holds a contract token can still fetch its own files while the local
// wisp-agent can fetch from any lease. Errors:
//
//   - 400 validation_error: bad path shape.
//   - 404 not_found: unknown contract, unknown path, or the path resolves to a
//     directory or symlink (both are rejected before any bytes leave the daemon).
//   - 409 lease_not_ready: contract not in ready/expiring.
//   - 413 file_too_large: file exceeds the download cap (default 16 MiB,
//     configurable via WISP_MAX_FILE_READ_BYTES).
//
// Content-Length is set from the tar header's declared size so a caller can
// stream the response without buffering it. The 200 header + Content-Length are
// committed before the first byte, so a mid-stream copy failure surfaces as a
// truncated body rather than an altered status code; the daemon-side read
// error is logged.
func (b *broker) fileDownload(w http.ResponseWriter, r *http.Request) {
	c, err := b.store.Get(r.PathValue("id"))
	if errors.Is(err, contract.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contract not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read contract")
		return
	}
	if !b.authorizedForContract(r, c) {
		writeError(w, http.StatusUnauthorized, "missing or invalid app or contract token")
		return
	}
	if c.State != contract.StateReady && c.State != contract.StateExpiring {
		writeError(w, http.StatusConflict, "lease_not_ready")
		return
	}
	path := r.URL.Query().Get("path")
	if err := validateFilePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := b.rt.CopyFileFromContainer(r.Context(), c.ContainerID, path)
	if err != nil {
		switch {
		case errors.Is(err, runtime.ErrNotFound),
			errors.Is(err, runtime.ErrFileIsDirectory),
			errors.Is(err, runtime.ErrFileIsSymlink),
			errors.Is(err, runtime.ErrFileNotRegular):
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		b.logger.Error("file download", "contract_id", c.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "wisp_error")
		return
	}
	defer res.Body.Close()

	limit := b.maxFileReadBytes
	if limit <= 0 {
		limit = defaultMaxFileReadBytes
	}
	if res.Size > limit {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file_too_large: %d bytes exceeds cap %d", res.Size, limit))
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	if res.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(res.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	// LimitReader keeps us honest at the header size even if the runtime's
	// underlying tar body contains padding past the file's declared bytes.
	if _, err := io.Copy(w, io.LimitReader(res.Body, res.Size)); err != nil {
		b.logger.Error("file download stream", "contract_id", c.ID, "error", err)
		// The 200 has already been committed, so we cannot change the status;
		// the body is truncated and the client sees a Content-Length mismatch.
	}
}
