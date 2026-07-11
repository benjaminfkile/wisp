package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/preset"
	"github.com/benjaminfkile/wisp/internal/runtime"
)

func TestHealthz(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), contract.NewStore(), runtime.NewFake(), preset.Builtin(), "")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok (body: %s)", body["status"], rec.Body.String())
	}
}

func TestHealthzRejectsPost(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), contract.NewStore(), runtime.NewFake(), preset.Builtin(), "")

	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
