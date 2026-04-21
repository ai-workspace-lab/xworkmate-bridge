package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"xworkmate-bridge/internal/service"
)

func TestTokenAuthHandlerServeHTTP(t *testing.T) {
	h := NewTokenAuthHandler(service.NewStaticTokenAuthService("secret"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "secret")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestTokenAuthHandlerRejectsUnauthorized(t *testing.T) {
	h := NewTokenAuthHandler(service.NewStaticTokenAuthService("secret"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
