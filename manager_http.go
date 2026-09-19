package raft

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
)

// RequestAuthorizer decides whether an HTTP request may act on a Manager. It
// returns nil to allow the request and an error to refuse it. The error is
// logged but never sent to the client, so it can name the reason freely.
type RequestAuthorizer func(*http.Request) error

// HandlerOption configures the handler returned by Manager.Handler.
type HandlerOption func(*handlerConfig)

type handlerConfig struct {
	authorize         RequestAuthorizer
	insecureAcknowled bool
}

// WithRequestAuthorizer requires every request to this handler to be authorized
// by fn.
//
// Without it the endpoints are open to anyone who can reach the listener, and
// one of them moves leadership. See Manager.Handler.
func WithRequestAuthorizer(fn RequestAuthorizer) HandlerOption {
	return func(c *handlerConfig) {
		if fn != nil {
			c.authorize = fn
		}
	}
}

// WithInsecureHandlerAcknowledged silences the warning that is otherwise logged
// when a handler is served with no authorizer. Use it when the listener is
// genuinely unreachable from outside a trusted boundary, so that the warning
// stays meaningful everywhere else.
func WithInsecureHandlerAcknowledged() HandlerOption {
	return func(c *handlerConfig) { c.insecureAcknowled = true }
}

// BearerTokenAuthorizer authorizes requests carrying an Authorization header of
// "Bearer <token>". The comparison is constant-time.
//
// This is a floor, not a complete answer: a bearer token sent over plaintext
// HTTP is readable by anything on the path. Serve the handler over TLS, or put
// a proxy that does in front of it.
func BearerTokenAuthorizer(token string) RequestAuthorizer {
	want := []byte("Bearer " + token)
	return func(r *http.Request) error {
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			return errors.New("raft: invalid or missing bearer token")
		}
		return nil
	}
}

// Handler returns an http.Handler that exposes two endpoints for remote status
// collection and leadership transfer:
//
//   - GET  /status   — returns Manager.StatusAll() as a JSON array of GroupStatus.
//   - POST /transfer — accepts {"group_id": N, "to": "nodeID"} and calls
//     TransferGroupLeadership. Returns 204 on success, 404 if the group is not
//     registered, or 500 for other errors.
//
// Together these let a remote BalanceController build a global view and execute
// transfers without in-process access to the Manager.
//
// # Access
//
// These endpoints are not read-only. Anyone who can reach /transfer can move
// leadership of any group to any member, repeatedly, which is enough to keep a
// cluster permanently mid-election. Pass WithRequestAuthorizer to require
// credentials. A handler served without one logs a warning at startup, which
// WithInsecureHandlerAcknowledged silences for the case where the listener is
// genuinely inside a trusted boundary.
func (m *Manager) Handler(opts ...HandlerOption) http.Handler {
	cfg := &handlerConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.authorize == nil && !cfg.insecureAcknowled {
		warnInsecureManagerHandler.Do(func() {
			slog.Warn("manager HTTP endpoints are being served WITHOUT authorization: " +
				"anyone who can reach this listener can move leadership of any group. " +
				"Pass raft.WithRequestAuthorizer, or raft.WithInsecureHandlerAcknowledged " +
				"if the listener is not reachable from outside a trusted boundary")
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", cfg.guard(m.handleStatus))
	mux.HandleFunc("POST /transfer", cfg.guard(m.handleTransfer))
	return mux
}

// warnInsecureManagerHandler keeps the unauthenticated-handler warning to one
// per process, so that a manager serving many groups does not drown its own log.
var warnInsecureManagerHandler sync.Once

// guard wraps h with the configured authorizer, if any.
func (c *handlerConfig) guard(h http.HandlerFunc) http.HandlerFunc {
	if c.authorize == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if err := c.authorize(r); err != nil {
			// The reason goes to the log, not to the caller: telling an
			// unauthorized client why it failed helps it succeed next time.
			slog.Warn("manager HTTP: request refused", "path", r.URL.Path, "err", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (m *Manager) handleStatus(w http.ResponseWriter, _ *http.Request) {
	statuses := m.StatusAll()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(statuses); err != nil {
		slog.Error("manager HTTP: status encode failed", "err", err)
	}
}

// transferBody is the JSON body accepted by POST /transfer.
type transferBody struct {
	GroupID uint64 `json:"group_id"`
	To      NodeID `json:"to"`
}

func (m *Manager) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var body transferBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.GroupID == 0 || body.To == "" {
		writeJSONError(w, http.StatusBadRequest, "group_id and to are required")
		return
	}

	if err := m.TransferGroupLeadership(r.Context(), body.GroupID, body.To); err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}
