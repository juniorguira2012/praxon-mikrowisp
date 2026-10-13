package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/axentra/mikrowisp-proxy/internal/mikrowisp"
)

// Server is the HTTP handler that fronts upstream Mikrowisp. Wire it into
// any http.ServeMux/http.Server; it serves all requests via ServeHTTP.
type Server struct {
	upstream *Upstream
	filter   *Filter
	log      *slog.Logger
}

// NewServer wires the handler. The Upstream and Filter come from main.go
// where they're constructed from the config.
func NewServer(upstream *Upstream, filter *Filter, log *slog.Logger) *Server {
	return &Server{upstream: upstream, filter: filter, log: log}
}

// ServeHTTP routes the inbound request to the right handler based on its
// path. Unknown paths fall through to genericForward — we never block a
// path because Mikrowisp may add new endpoints we should still pass on.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Mikrowisp's API is POST-only. Anything else is almost certainly
		// a misconfiguration; respond with a clear hint instead of
		// silently forwarding.
		http.Error(w, `{"estado":"error","mensaje":"proxy: only POST is supported"}`, http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.log.Warn("read inbound body", slog.String("err", err.Error()))
		http.Error(w, `{"estado":"error","mensaje":"proxy: cannot read body"}`, http.StatusBadRequest)
		return
	}

	switch r.URL.Path {
	case mikrowisp.GetClientsDetailsPath:
		s.handleGetClientsDetails(w, r, body)
	case mikrowisp.GetInvoicesPath:
		s.handleGetInvoices(w, r, body)
	default:
		s.genericForward(w, r, body)
	}
}

// genericForward sends the body upstream untouched and writes the upstream
// response back to the caller. Used for Ping, PaidInvoice, and any
// endpoint we haven't taught the proxy to filter on.
func (s *Server) genericForward(w http.ResponseWriter, r *http.Request, body []byte) {
	status, raw, err := s.upstream.Forward(r.Context(), r.URL.Path, body)
	if err != nil {
		s.log.Warn("upstream forward failed",
			slog.String("path", r.URL.Path),
			slog.String("err", err.Error()))
		http.Error(w, `{"estado":"error","mensaje":"proxy: upstream unreachable"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// handleGetClientsDetails forwards the request, then if the response is a
// success envelope it walks the `datos` array and drops every entry whose
// extras match an exclusion rule.
//
// Mikrowisp returns clients identified by `id` (int) at the top of each
// datos entry. We need that id to seed the verdict cache so a subsequent
// GetInvoices request short-circuits without another round-trip.
func (s *Server) handleGetClientsDetails(w http.ResponseWriter, r *http.Request, body []byte) {
	status, raw, err := s.upstream.Forward(r.Context(), r.URL.Path, body)
	if err != nil {
		s.log.Warn("upstream GetClientsDetails", slog.String("err", err.Error()))
		http.Error(w, `{"estado":"error","mensaje":"proxy: upstream unreachable"}`, http.StatusBadGateway)
		return
	}

	// Parse just enough to introspect: envelope + datos as a slice of
	// generic JSON objects. We keep the raw bytes around so we can
	// re-emit unchanged keys for surviving rows.
	var resp struct {
		Estado  string                       `json:"estado"`
		Mensaje string                       `json:"mensaje"`
		Datos   []map[string]json.RawMessage `json:"datos"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		s.log.Warn("parse GetClientsDetails", slog.String("err", err.Error()))
		// On parse failure, pass the raw bytes through. Better to let
		// the caller see the upstream's response than to invent one.
		writePassthrough(w, status, raw)
		return
	}
	if resp.Estado != "exito" {
		writePassthrough(w, status, raw)
		return
	}

	kept := make([]map[string]json.RawMessage, 0, len(resp.Datos))
	dropped := 0
	for _, d := range resp.Datos {
		idCliente := readClientID(d)
		if s.filter.FilterClient(idCliente, d) {
			dropped++
			continue
		}
		kept = append(kept, d)
	}

	if dropped == 0 {
		// Nothing to filter; preserve byte-for-byte fidelity with
		// upstream so callers comparing checksums don't see drift.
		writePassthrough(w, status, raw)
		return
	}

	s.log.Info("filtered GetClientsDetails",
		slog.Int("kept", len(kept)),
		slog.Int("dropped", dropped))

	out, err := json.Marshal(map[string]any{
		"estado":  resp.Estado,
		"mensaje": resp.Mensaje,
		"datos":   kept,
	})
	if err != nil {
		// Should be impossible — we just unmarshaled the same shape.
		// Surface clearly rather than corrupting the response.
		s.log.Error("re-marshal GetClientsDetails failed", slog.String("err", err.Error()))
		http.Error(w, `{"estado":"error","mensaje":"proxy: internal re-encode failure"}`, http.StatusInternalServerError)
		return
	}
	writePassthrough(w, status, out)
}

// handleGetInvoices forwards the request, then if the response is a
// success envelope it walks the `facturas` array and drops every invoice
// whose `idcliente` resolves to an excluded customer.
//
// To decide, the filter consults its verdict cache; on miss it calls
// upstream GetClientsDetails for the missing client (one-shot per unique
// id per request). For 100 invoices belonging to 30 clients, that's at
// most 30 extra upstream calls on a cold cache — and zero on a warm one.
func (s *Server) handleGetInvoices(w http.ResponseWriter, r *http.Request, body []byte) {
	status, raw, err := s.upstream.Forward(r.Context(), r.URL.Path, body)
	if err != nil {
		s.log.Warn("upstream GetInvoices", slog.String("err", err.Error()))
		http.Error(w, `{"estado":"error","mensaje":"proxy: upstream unreachable"}`, http.StatusBadGateway)
		return
	}

	// Token comes back to us via the inbound request body; we need it
	// to make the cache-miss GetClientsDetails calls.
	token := readToken(body)

	var resp struct {
		Estado   string            `json:"estado"`
		Mensaje  string            `json:"mensaje"`
		Facturas []json.RawMessage `json:"facturas"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		s.log.Warn("parse GetInvoices", slog.String("err", err.Error()))
		writePassthrough(w, status, raw)
		return
	}
	if resp.Estado != "exito" {
		writePassthrough(w, status, raw)
		return
	}

	kept := make([]json.RawMessage, 0, len(resp.Facturas))
	dropped := 0
	for _, inv := range resp.Facturas {
		var probe struct {
			IDCliente int `json:"idcliente"`
		}
		// Best-effort id extraction. An invoice with no idcliente in
		// the row (malformed upstream data) cannot be filtered, so we
		// keep it — the caller decides what to do.
		_ = json.Unmarshal(inv, &probe)
		excluded, err := s.filter.IsExcluded(r.Context(), token, probe.IDCliente)
		if err != nil {
			// Fail-closed on filter errors: dropping rows we can't
			// classify would silently under-emit; keeping them might
			// over-emit. We choose to surface the error to the caller
			// so the operator notices, rather than guessing.
			s.log.Warn("filter check failed",
				slog.Int("idcliente", probe.IDCliente),
				slog.String("err", err.Error()))
			http.Error(w, `{"estado":"error","mensaje":"proxy: filter check failed"}`, http.StatusBadGateway)
			return
		}
		if excluded {
			dropped++
			continue
		}
		kept = append(kept, inv)
	}

	if dropped == 0 {
		writePassthrough(w, status, raw)
		return
	}

	s.log.Info("filtered GetInvoices",
		slog.Int("kept", len(kept)),
		slog.Int("dropped", dropped))

	out, err := json.Marshal(map[string]any{
		"estado":   resp.Estado,
		"mensaje":  resp.Mensaje,
		"facturas": kept,
	})
	if err != nil {
		s.log.Error("re-marshal GetInvoices failed", slog.String("err", err.Error()))
		http.Error(w, `{"estado":"error","mensaje":"proxy: internal re-encode failure"}`, http.StatusInternalServerError)
		return
	}
	writePassthrough(w, status, out)
}

// readClientID pulls the integer `id` field from a client details object.
// Returns 0 on missing/malformed input — callers handle 0 as "no id".
func readClientID(d map[string]json.RawMessage) int {
	raw, ok := d["id"]
	if !ok {
		return 0
	}
	var id int
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0
	}
	return id
}

// readToken pulls the `token` field from the inbound request body. We
// need it to make secondary GetClientsDetails calls during invoice
// filtering. The token is held only in this function's stack memory and
// never logged or persisted.
func readToken(body []byte) string {
	var probe struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Token)
}

// writePassthrough writes the raw upstream bytes back to the caller with
// the upstream status code. Centralized so we don't drift between
// happy-path and error-path response handling.
func writePassthrough(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

