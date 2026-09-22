package raft_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brunoga/raft/v2"
)

// TestManagerHandler_AuthorizerGuardsEveryRoute asserts that when an authorizer
// is configured, no route can be reached without passing it.
//
// These endpoints are not read-only: one of them moves leadership of any group
// to any member. Anyone who can reach it can keep a cluster permanently
// mid-election, which needs no access to the data to be a serious problem.
func TestManagerHandler_AuthorizerGuardsEveryRoute(t *testing.T) {
	mgr := raft.NewManager()
	t.Cleanup(mgr.StopAll)

	handler := mgr.Handler(raft.WithRequestAuthorizer(raft.BearerTokenAuthorizer("s3cret")))

	routes := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/status", ""},
		{http.MethodPost, "/transfer", `{"group_id":1,"to":"n2"}`},
	}
	credentials := []struct {
		name, header string
	}{
		{"no header", ""},
		{"wrong token", "Bearer wrong"},
		{"wrong scheme", "Basic s3cret"},
		{"token without scheme", "s3cret"},
		{"prefix of the token", "Bearer s3cre"},
	}

	for _, route := range routes {
		for _, cred := range credentials {
			t.Run(route.path+"/"+cred.name, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				if cred.header != "" {
					req.Header.Set("Authorization", cred.header)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s %s with %s: status %d, want 401",
						route.method, route.path, cred.name, rec.Code)
				}
			})
		}
	}
}

// TestManagerHandler_AuthorizedRequestIsServed is the companion: a request that
// passes the authorizer reaches the handler.
func TestManagerHandler_AuthorizedRequestIsServed(t *testing.T) {
	mgr := raft.NewManager()
	t.Cleanup(mgr.StopAll)

	handler := mgr.Handler(raft.WithRequestAuthorizer(raft.BearerTokenAuthorizer("s3cret")))

	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authorized request got status %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.HasPrefix(strings.TrimSpace(got), "[") {
		t.Errorf("body = %q, want a JSON array of group statuses", got)
	}
}

// TestManagerHandler_RefusalDoesNotLeakTheReason asserts that the response to a
// refused request says nothing a caller could use to get closer to a valid
// credential.
func TestManagerHandler_RefusalDoesNotLeakTheReason(t *testing.T) {
	mgr := raft.NewManager()
	t.Cleanup(mgr.StopAll)

	handler := mgr.Handler(raft.WithRequestAuthorizer(func(*http.Request) error {
		return errors.New("token expired at 12:04 for user alice from 10.0.0.7")
	}))

	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, leak := range []string{"expired", "alice", "10.0.0.7", "12:04"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response body leaked %q from the authorizer's error: %q", leak, rec.Body.String())
		}
	}
}

// TestManagerHandler_WithoutAuthorizerRefuses pins that a handler given
// neither an authorizer nor the acknowledgement serves nothing: every request
// is answered 403, so an endpoint nobody meant to leave open is never open by
// accident.
func TestManagerHandler_WithoutAuthorizerRefuses(t *testing.T) {
	mgr := raft.NewManager()
	t.Cleanup(mgr.StopAll)

	handler := mgr.Handler()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/status"},
		{http.MethodPost, "/transfer"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"group_id":1,"to":"n2"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s on an unconfigured handler returned %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

// TestManagerHandler_AcknowledgedInsecureServes pins that the acknowledgement
// is what turns the open handler on.
func TestManagerHandler_AcknowledgedInsecureServes(t *testing.T) {
	mgr := raft.NewManager()
	t.Cleanup(mgr.StopAll)

	handler := mgr.Handler(raft.WithInsecureHandlerAcknowledged())

	req := httptest.NewRequest(http.MethodGet, "/status", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("unauthenticated handler returned status %d, want 200", rec.Code)
	}
}
