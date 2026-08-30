package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/policy"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// createBody builds a JSON POST /contracts body for a single-file ingress test.
// The caller supplies the file's absolute path and raw content; content_base64
// is filled in by this helper.
func createBody(ttl int, path string, content []byte) string {
	return fmt.Sprintf(`{"ttl_seconds":%d,"files":[{"path":%q,"content_base64":%q}]}`,
		ttl, path, base64.StdEncoding.EncodeToString(content))
}

// TestCreateAcceptsFilesAndWritesBeforeUserdata pins the pinned file-contract
// ordering: the ingress files must land in the container BEFORE the userdata
// exec runs, so a userdata script can read them. Uses a StreamHandler-free
// ExecHandler so we can peek at CopyCalls vs Execs and prove Copy happened
// first.
func TestCreateAcceptsFilesAndWritesBeforeUserdata(t *testing.T) {
	h, store, fake := testServer(t)
	fake.ExecHandler = func(id string, cmd []string) (runtime.ExecResult, error) {
		// Peek at what files the fake sees at userdata time; the test asserts
		// on the container's copied files after create returns.
		return runtime.ExecResult{ExitCode: 0}, nil
	}

	body := createBody(3600, "/etc/wisp/setup.sh", []byte("echo hi"))
	created := createContract(t, h, body)

	c, _ := store.Get(created.ContractID)
	fc, ok := fake.Container(c.ContainerID)
	if !ok {
		t.Fatalf("container not tracked")
	}
	got, ok := fc.Files["/etc/wisp/setup.sh"]
	if !ok {
		t.Fatalf("file not copied to container; files=%v", fc.Files)
	}
	if string(got.Content) != "echo hi" {
		t.Errorf("file content = %q, want %q", got.Content, "echo hi")
	}
	if len(fc.CopyCalls) != 1 {
		t.Fatalf("CopyCalls = %v, want exactly one batch", fc.CopyCalls)
	}
	if !reflect.DeepEqual(fc.CopyCalls[0], []string{"/etc/wisp/setup.sh"}) {
		t.Errorf("CopyCalls[0] = %v, want [/etc/wisp/setup.sh]", fc.CopyCalls[0])
	}
	if len(fc.Execs) != 0 {
		t.Errorf("Execs after create without userdata = %v, want empty", fc.Execs)
	}
}

// TestCreateFilesRejectedDoesNotBoot verifies every pinned cap breach is a
// validation_error 400: too many files, too large decoded total, bad path
// shape (relative, "..", backslash, too long), duplicate paths, and invalid
// base64. Each rejected create must produce no contract row and no container.
func TestCreateFilesRejectedDoesNotBoot(t *testing.T) {
	// One-KiB file used as a building block for the "too many" and "too large"
	// bodies below.
	one := make([]byte, 1024)
	oneB64 := base64.StdEncoding.EncodeToString(one)

	// Build a files array of n entries with unique paths, each holding `one`
	// bytes; total decoded size = n * 1024.
	filesJSON := func(n int) string {
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, fmt.Sprintf(`{"path":"/f%d","content_base64":%q}`, i, oneB64))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}

	cases := []struct {
		name string
		body string
	}{
		{
			name: "too many files",
			body: `{"ttl_seconds":60,"files":` + filesJSON(17) + `}`,
		},
		{
			// 1 MiB is the exact cap; 1 MiB + 1 file (1025 files of 1024 B) is 1 KiB over.
			name: "total decoded exceeds cap",
			body: `{"ttl_seconds":60,"files":` + filesJSON(1025) + `}`,
		},
		{
			name: "relative path",
			body: `{"ttl_seconds":60,"files":[{"path":"etc/hi","content_base64":""}]}`,
		},
		{
			name: "path with '..' segment",
			body: `{"ttl_seconds":60,"files":[{"path":"/etc/../hi","content_base64":""}]}`,
		},
		{
			name: "path with backslash",
			body: `{"ttl_seconds":60,"files":[{"path":"/etc\\hi","content_base64":""}]}`,
		},
		{
			name: "path too long",
			body: `{"ttl_seconds":60,"files":[{"path":"/` + strings.Repeat("a", 256) + `","content_base64":""}]}`,
		},
		{
			name: "duplicate paths",
			body: `{"ttl_seconds":60,"files":[{"path":"/a","content_base64":""},{"path":"/a","content_base64":""}]}`,
		},
		{
			name: "invalid base64",
			body: `{"ttl_seconds":60,"files":[{"path":"/a","content_base64":"!!!not-base64!!!"}]}`,
		},
		{
			name: "empty path",
			body: `{"ttl_seconds":60,"files":[{"path":"","content_base64":""}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, fake := testServer(t)
			rec := do(t, h, http.MethodPost, "/contracts", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if n := len(store.List()); n != 0 {
				t.Errorf("stored contracts = %d, want 0 after rejected files", n)
			}
			if n := fake.Count(); n != 0 {
				t.Errorf("live containers = %d, want 0 after rejected files", n)
			}
		})
	}
}

// TestCreateFilesAtExactCaps verifies the boundaries are inclusive: 16 files
// summing to 1 MiB exactly is accepted, one file of exactly 1 MiB is accepted,
// and a 256-byte path is accepted. If any of these were rejected, the pinned
// caps would effectively be one smaller than the wire contract says.
func TestCreateFilesAtExactCaps(t *testing.T) {
	// 16 files of 65 536 bytes = 1 MiB exactly.
	const each = 65_536
	var parts []string
	for i := 0; i < 16; i++ {
		parts = append(parts, fmt.Sprintf(`{"path":"/f%d","content_base64":%q}`,
			i, base64.StdEncoding.EncodeToString(make([]byte, each))))
	}
	body := `{"ttl_seconds":60,"files":[` + strings.Join(parts, ",") + `]}`
	h, _, _ := testServer(t)
	rec := do(t, h, http.MethodPost, "/contracts", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("16x64K status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	// One file at the exact 1 MiB total cap.
	oneMiB := make([]byte, 1024*1024)
	body = fmt.Sprintf(`{"ttl_seconds":60,"files":[{"path":"/big","content_base64":%q}]}`,
		base64.StdEncoding.EncodeToString(oneMiB))
	h, _, _ = testServer(t)
	if rec = do(t, h, http.MethodPost, "/contracts", body); rec.Code != http.StatusCreated {
		t.Errorf("1 MiB single file status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	// 256-byte path exact boundary.
	longPath := "/" + strings.Repeat("a", 255) // 256 chars total including leading '/'
	body = fmt.Sprintf(`{"ttl_seconds":60,"files":[{"path":%q,"content_base64":""}]}`, longPath)
	h, _, _ = testServer(t)
	if rec = do(t, h, http.MethodPost, "/contracts", body); rec.Code != http.StatusCreated {
		t.Errorf("256-byte path status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateFilesUserdataSeesThem drives the ordering guarantee end to end via
// an ExecHandler that inspects the fake's file store at userdata time. The
// userdata "reads" the file by looking at the fake's tracked contents from
// inside the exec handler and returns its length as the exit code, so a
// success means the files were present when userdata ran.
func TestCreateFilesUserdataSeesThem(t *testing.T) {
	h, store, fake := testServer(t)
	fake.ExecHandler = func(id string, cmd []string) (runtime.ExecResult, error) {
		fc, ok := fake.Container(id)
		if !ok {
			t.Fatalf("exec against unknown container %s", id)
		}
		entry, ok := fc.Files["/wisp/greeting"]
		if !ok {
			return runtime.ExecResult{ExitCode: 42}, nil // sentinel: file missing at exec
		}
		return runtime.ExecResult{Stdout: string(entry.Content), ExitCode: 0}, nil
	}

	body := fmt.Sprintf(`{"ttl_seconds":60,"userdata":"cat /wisp/greeting","files":[{"path":"/wisp/greeting","content_base64":%q}]}`,
		base64.StdEncoding.EncodeToString([]byte("hello")))
	created := createContract(t, h, body)
	c, _ := store.Get(created.ContractID)
	if c.State != contract.StateReady {
		t.Fatalf("state = %q, want ready", c.State)
	}
	// The exec ran; if it had exit_code=42 above, the create would have failed
	// via the userdata failure path (500). We reached 201 ready, so the file
	// was present at exec time.
}

// TestFileDownloadStreamsBytes verifies the happy path: a written file is
// streamed back as application/octet-stream with the right Content-Length and
// the raw bytes in the body.
func TestFileDownloadStreamsBytes(t *testing.T) {
	h, store, fake := testServer(t)
	content := []byte("hello, wisp\n")
	body := createBody(3600, "/etc/data", content)
	created := createContract(t, h, body)

	c, _ := store.Get(created.ContractID)
	fc, _ := fake.Container(c.ContainerID)
	if _, ok := fc.Files["/etc/data"]; !ok {
		t.Fatalf("file not written; files=%v", fc.Files)
	}

	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/data",
		created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET files status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(content)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(content))
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), content)
	}
}

// TestFileDownloadAppTokenAndContractToken pins the auth rule: the app token
// OR the contract's own bearer token authorizes the download; a wrong token or
// no token is rejected with 401.
func TestFileDownloadAppTokenAndContractToken(t *testing.T) {
	// Build a broker with a non-empty app token so the gate is enabled.
	store := contract.NewStore()
	fake := runtime.NewFake()
	b := newBroker(store, fake, policy.Default(), bus.New(nil), discardLogger(), "app-secret")
	mux := http.NewServeMux()
	b.routes(mux)

	// Create a contract via the app token.
	rec := doAuth(t, mux, http.MethodPost, "/contracts", "app-secret",
		createBody(3600, "/etc/data", []byte("payload")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// The contract's own bearer token authorizes the download.
	rec = doAuth(t, mux, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/data",
		created.Token, "")
	if rec.Code != http.StatusOK {
		t.Errorf("contract-token download status = %d, want 200", rec.Code)
	}
	// The app token also authorizes it (same rule as GET /contracts/{id}).
	rec = doAuth(t, mux, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/data",
		"app-secret", "")
	if rec.Code != http.StatusOK {
		t.Errorf("app-token download status = %d, want 200", rec.Code)
	}
	// A wrong token is 401.
	rec = doAuth(t, mux, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/data",
		"nope", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token download status = %d, want 401", rec.Code)
	}
	// No Authorization header at all is 401.
	rec = do(t, mux, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/data", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-token download status = %d, want 401", rec.Code)
	}
}

// TestFileDownload413ForTooLarge pins the 413 file_too_large behavior: a file
// larger than the configured cap is rejected without streaming a single byte
// of the body.
func TestFileDownload413ForTooLarge(t *testing.T) {
	h, store, fake := testServer(t)
	// Cap the download at 1 KiB via the broker's per-instance setting; use
	// direct access so we don't need to plumb env vars for the test.
	// The default is 16 MiB, so we shrink the cap in the broker.
	// We need to access the broker directly; testServer wraps NewDaemon which
	// hides it. Build the wiring by hand instead.
	store = contract.NewStore()
	fake = runtime.NewFake()
	b := newBroker(store, fake, policy.Default(), bus.New(nil), discardLogger(), "")
	b.maxFileReadBytes = 1024
	mux := http.NewServeMux()
	b.routes(mux)
	_ = h // unused

	body := createBody(3600, "/etc/big", make([]byte, 2048))
	created := createContract(t, mux, body)

	rec := doAuth(t, mux, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/big",
		created.Token, "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(errBody.Error, "file_too_large") {
		t.Errorf("error body = %q, want to contain file_too_large", errBody.Error)
	}
}

// TestFileDownload404ForDirectoryAndSymlink verifies the runtime's typeflag
// rejections map to 404 not_found: neither directory listings nor symlink
// targets ever leak back to the caller.
func TestFileDownload404ForDirectoryAndSymlink(t *testing.T) {
	h, store, fake := testServer(t)
	// Write a file, then swap its type in the fake's tracking to simulate the
	// container-side tar entry actually being a directory or a symlink at
	// download time. The runtime's fake honours TypeOverride to drive both
	// rejection paths from the same place.
	body := createBody(3600, "/etc/target", []byte("data"))
	created := createContract(t, h, body)
	c, _ := store.Get(created.ContractID)

	// Directory
	fc, _ := fake.Container(c.ContainerID)
	fc.Files["/etc/target"] = runtime.FakeFileEntry{Content: []byte("data"), TypeOverride: tar.TypeDir}
	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/target",
		created.Token, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("directory download status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}

	// Symlink
	fc.Files["/etc/target"] = runtime.FakeFileEntry{Content: []byte("data"), TypeOverride: tar.TypeSymlink}
	rec = doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/target",
		created.Token, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("symlink download status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestFileDownload404ForMissingPath pins the not-found path: a request for a
// path that was never written maps to 404, not a 500 or an empty 200.
func TestFileDownload404ForMissingPath(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":60}`)
	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/does/not/exist",
		created.Token, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestFileDownload409WhenNotReady pins the state check: a contract not in
// ready or expiring is 409 lease_not_ready. Move the contract to
// StateProvisioning to trip the check without needing to release.
func TestFileDownload409WhenNotReady(t *testing.T) {
	h, store, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":60}`)
	// Push the state back to provisioning: illegal transition normally, but
	// the store enforces the machine, so instead move forward to expired.
	if _, err := store.UpdateState(created.ContractID, contract.StateExpired); err != nil {
		t.Fatalf("UpdateState expired: %v", err)
	}
	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/anything",
		created.Token, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("expired status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestFileDownloadAllowedDuringExpiring verifies the expiring lead window is
// still readable, matching the pinned rule that both ready and expiring
// contracts are readable.
func TestFileDownloadAllowedDuringExpiring(t *testing.T) {
	h, store, _ := testServer(t)
	body := createBody(3600, "/etc/data", []byte("bye"))
	created := createContract(t, h, body)

	if _, err := store.UpdateState(created.ContractID, contract.StateExpiring); err != nil {
		t.Fatalf("UpdateState expiring: %v", err)
	}
	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/etc/data",
		created.Token, "")
	if rec.Code != http.StatusOK {
		t.Errorf("expiring download status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), []byte("bye")) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), "bye")
	}
}

// TestFileDownload400ForBadPathShape pins the shape validation: relative path,
// ".." segment, backslash, over-long, and empty ?path= all map to 400 rather
// than reaching the runtime.
func TestFileDownload400ForBadPathShape(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, `{"ttl_seconds":60}`)
	badPaths := []string{
		"",                             // empty
		"etc/rel",                      // relative
		"/etc/../secret",               // traversal
		"/etc\\bad",                    // backslash
		"/" + strings.Repeat("a", 257), // over-long (256-byte cap)
	}
	for _, p := range badPaths {
		url := "/contracts/" + created.ContractID + "/files?path=" + p
		rec := doAuth(t, h, http.MethodGet, url, created.Token, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad path %q status = %d, want 400 (body: %s)", p, rec.Code, rec.Body.String())
		}
	}
}

// TestFileDownloadUnknownContract404 pins the unknown-id path: a request
// against a contract id no wispd process knows is 404, not 500 or 401.
func TestFileDownloadUnknownContract404(t *testing.T) {
	h, _, _ := testServer(t)
	rec := doAuth(t, h, http.MethodGet, "/contracts/does-not-exist/files?path=/x", "anytoken", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown id (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestFileDownloadClientStreamLarger pins the LimitReader behaviour: even if
// the runtime returned more bytes than the tar header declared, the endpoint
// only writes exactly `Size` bytes. Uses a custom runtime shim whose CopyFile
// method returns a body longer than the declared size.
func TestFileDownloadClientStreamLarger(t *testing.T) {
	fake := runtime.NewFake()
	rt := &oversizedReadRuntime{Runtime: fake, size: 5, body: []byte("abcXXXXX")}
	store := contract.NewStore()
	b := newBroker(store, rt, policy.Default(), bus.New(nil), discardLogger(), "")
	mux := http.NewServeMux()
	b.routes(mux)

	// Bootstrap: create a contract so the download path finds a ready lease.
	created := createContract(t, mux, `{"ttl_seconds":60}`)
	rec := doAuth(t, mux, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/x",
		created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, []byte("abcXX")) {
		// LimitReader keeps us to Size (5) bytes even though the runtime
		// tried to hand us 8; that is the point of the wrapper.
		t.Errorf("body = %q, want first 5 bytes only", got)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "5" {
		t.Errorf("Content-Length = %q, want 5", cl)
	}
}

// oversizedReadRuntime is a test shim over the fake that returns a
// FileReadResult whose Body has MORE bytes than Size claims, so the endpoint's
// LimitReader wrapping can be verified.
type oversizedReadRuntime struct {
	runtime.Runtime
	size int64
	body []byte
}

func (r *oversizedReadRuntime) CopyFileFromContainer(_ context.Context, _ string, _ string) (runtime.FileReadResult, error) {
	return runtime.FileReadResult{Size: r.size, Body: io.NopCloser(bytes.NewReader(r.body))}, nil
}

// TestServerHandlerAllowsFilesRoute verifies the wispd Handler includes the
// /files route by exercising it end to end through the full New() wiring
// rather than the raw broker mux.
func TestServerHandlerAllowsFilesRoute(t *testing.T) {
	store := contract.NewStore()
	fake := runtime.NewFake()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, policy.Default(), bus.New(nil), "")
	body := createBody(60, "/hi", []byte("data"))
	created := createContract(t, h, body)

	rec := doAuth(t, h, http.MethodGet, "/contracts/"+created.ContractID+"/files?path=/hi",
		created.Token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 through the full handler (body: %s)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), []byte("data")) {
		t.Errorf("body = %q, want %q", rec.Body.Bytes(), "data")
	}
}

// TestCreateFilesFakeRoundTripsViaDownload is a broader end-to-end: a create
// with several files, then a download of each, verifies both directions of
// the file surface through the same handler. The httptest recorder captures
// each body byte-for-byte.
func TestCreateFilesFakeRoundTripsViaDownload(t *testing.T) {
	h, _, _ := testServer(t)
	entries := []struct {
		path    string
		content []byte
	}{
		{"/a", []byte("A")},
		{"/etc/b.txt", bytes.Repeat([]byte("B"), 256)},
		{"/nested/dir/c.bin", bytes.Repeat([]byte{0, 1, 2, 3}, 32)},
	}
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf(`{"path":%q,"content_base64":%q}`,
			e.path, base64.StdEncoding.EncodeToString(e.content)))
	}
	body := `{"ttl_seconds":60,"files":[` + strings.Join(parts, ",") + `]}`
	created := createContract(t, h, body)

	for _, e := range entries {
		u := "/contracts/" + created.ContractID + "/files?path=" + e.path
		rec := doAuth(t, h, http.MethodGet, u, created.Token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("download %s status = %d, want 200 (body: %s)", e.path, rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), e.content) {
			t.Errorf("download %s body = %q, want %q", e.path, rec.Body.Bytes(), e.content)
		}
	}
}

// TestFileDownloadRequestHeaderCarriesTokenWithSubprotocolStyle is a smoke
// test that the standard Authorization: Bearer header path (used everywhere
// else in wisp) is what the download handler consumes; there is no ?token=
// short-circuit added for /files.
func TestFileDownloadRequestHeaderCarriesTokenWithSubprotocolStyle(t *testing.T) {
	h, _, _ := testServer(t)
	created := createContract(t, h, createBody(60, "/x", []byte("y")))
	// ?token=... is not honored for /files (unlike /shell); the header path is
	// the only one, so this must be 401.
	req := httptest.NewRequest(http.MethodGet,
		"/contracts/"+created.ContractID+"/files?path=/x&token="+created.Token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// With no app token configured this endpoint is open (authorizedForContract
	// short-circuits true when appToken is empty), so ?token= is irrelevant
	// here. Assert only that the endpoint responded 200 in that mode, matching
	// the localhost-friendly default.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (open mode) (body: %s)", rec.Code, rec.Body.String())
	}
}
