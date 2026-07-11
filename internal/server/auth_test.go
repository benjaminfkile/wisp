package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/preset"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

// tokenServer builds a handler whose contract-creation gate requires appToken.
func tokenServer(t *testing.T, appToken string) http.Handler {
	t.Helper()
	store := contract.NewStore()
	fake := runtime.NewFake()
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fake, preset.Builtin(), bus.New(nil), appToken)
}

func TestCreateRequiresAppToken(t *testing.T) {
	h := tokenServer(t, "app-secret")
	body := `{"ttl_seconds":3600,"preset":"coding"}`

	// No Authorization header at all.
	if rec := do(t, h, http.MethodPost, "/contracts", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing-token create status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}

	// Wrong token.
	if rec := doAuth(t, h, http.MethodPost, "/contracts", "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token create status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}

	// Correct token: creation proceeds.
	if rec := doAuth(t, h, http.MethodPost, "/contracts", "app-secret", body); rec.Code != http.StatusCreated {
		t.Errorf("good-token create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

// When no app token is configured the gate is disabled: creation works without
// any credential (the localhost-friendly default, see docs/DESIGN.md §8).
func TestCreateWithoutAppTokenIsOpen(t *testing.T) {
	h := tokenServer(t, "")
	rec := do(t, h, http.MethodPost, "/contracts", `{"ttl_seconds":3600,"preset":"coding"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("open create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

// The app token gates only creation; contract-scoped calls stay gated by the
// per-contract token. A contract created with the app token is then driven with
// its own bearer token, not the app token.
func TestAppTokenAndContractTokenAreDistinct(t *testing.T) {
	h := tokenServer(t, "app-secret")

	rec := doAuth(t, h, http.MethodPost, "/contracts", "app-secret",
		`{"ttl_seconds":3600,"preset":"coding"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created createResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// The app token must NOT authorize exec; only the contract token does.
	if r := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		"app-secret", `{"command":"echo hi"}`); r.Code != http.StatusUnauthorized {
		t.Errorf("exec with app token status = %d, want 401", r.Code)
	}
	if r := doAuth(t, h, http.MethodPost, "/contracts/"+created.ContractID+"/exec",
		created.Token, `{"command":"echo hi"}`); r.Code != http.StatusOK {
		t.Errorf("exec with contract token status = %d, want 200 (body: %s)", r.Code, r.Body.String())
	}
}

// requireAppToken is unit-tested directly: it passes through when the token
// matches or the gate is disabled, and short-circuits with 401 otherwise.
func TestRequireAppTokenMiddleware(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }

	cases := []struct {
		name     string
		appToken string
		header   string
		want     int
	}{
		{"disabled passes without header", "", "", http.StatusTeapot},
		{"disabled passes with header", "", "Bearer anything", http.StatusTeapot},
		{"enabled rejects missing header", "sekret", "", http.StatusUnauthorized},
		{"enabled rejects wrong token", "sekret", "Bearer nope", http.StatusUnauthorized},
		{"enabled rejects non-bearer scheme", "sekret", "Basic sekret", http.StatusUnauthorized},
		{"enabled passes matching token", "sekret", "Bearer sekret", http.StatusTeapot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &broker{appToken: tc.appToken}
			h := b.requireAppToken(ok)
			req := httptest.NewRequest(http.MethodPost, "/contracts", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
