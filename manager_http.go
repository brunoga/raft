package raft

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Handler returns an http.Handler that exposes two endpoints for remote
// status collection and leadership transfer:
//
//   - GET  /status   — returns Manager.StatusAll() as a JSON array of GroupStatus.
//   - POST /transfer — accepts {"group_id": N, "to": "nodeID"} and calls
//     TransferGroupLeadership. Returns 204 on success, 404 if the group is not
//     registered, or 500 for other errors.
//
// This satisfies §22 Step 2: a per-physical-node HTTP endpoint that allows a
// remote BalanceController (via HTTPNodeProvider) to build a global view and
// execute transfers without direct in-process access to the Manager.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", m.handleStatus)
	mux.HandleFunc("POST /transfer", m.handleTransfer)
	return mux
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
