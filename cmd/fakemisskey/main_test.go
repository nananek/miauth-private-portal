package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHandleMiAuthStart_RedirectsToCallbackWithSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /miauth/{id}", handleMiAuthStart)

	callback := "http://localhost:8080/miauth/callback?id=abc&state=def"
	req := httptest.NewRequest(http.MethodGet, "/miauth/upstream-1?name=miauth-private-portal&permission=read:account&callback="+url.QueryEscape(callback), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location header: %v", err)
	}
	q := loc.Query()
	if q.Get("id") != "abc" || q.Get("state") != "def" {
		t.Fatalf("redirect query = %v, want id/state preserved from callback", q)
	}
	if q.Get("session") != "upstream-1" {
		t.Fatalf("session = %q, want %q", q.Get("session"), "upstream-1")
	}
}

func TestHandleMiAuthStart_MissingCallbackIsBadRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /miauth/{id}", handleMiAuthStart)

	req := httptest.NewRequest(http.MethodGet, "/miauth/upstream-1?permission=read:account", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleCheck_ReturnsFixedUser(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/miauth/{id}/check", handleCheck("contract-test-owner"))

	req := httptest.NewRequest(http.MethodPost, "/api/miauth/upstream-1/check", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp checkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.Token == "" || resp.User.ID != "contract-test-owner" {
		t.Fatalf("response = %+v, want ok=true, non-empty token, user.id=contract-test-owner", resp)
	}
}
