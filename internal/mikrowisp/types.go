// Package mikrowisp models the request and response shapes the proxy
// inspects. We intentionally model only the fields we filter on; every
// other field passes through transparently as raw JSON via
// `json.RawMessage` or `json.Unmarshal` round-trips.
//
// Response envelopes follow Mikrowisp's documented convention:
//
//	{ "estado": "exito" | "error", "mensaje": "...", ...payload }
//
// On `estado != "exito"` we forward the raw upstream response without
// applying any filter — the body is an error and has no rows to filter.
package mikrowisp

import "encoding/json"

// Envelope is the prefix every Mikrowisp response carries. The payload
// fields (facturas, datos, etc.) live alongside it in the JSON object.
type Envelope struct {
	Estado  string `json:"estado"`
	Mensaje string `json:"mensaje,omitempty"`
}

// Invoice is one entry of the `facturas` array on GetInvoices. Only the
// fields the proxy needs to make filtering decisions are typed; the rest
// of the row is preserved in Raw and re-emitted unchanged.
type Invoice struct {
	IDCliente int             `json:"idcliente"`
	Raw       json.RawMessage `json:"-"`
}

// InvoicesResponse is the GetInvoices response shape. Same envelope plus
// the `facturas` array. We keep the array untyped (RawMessage entries) so
// any future Mikrowisp field additions ride through without a code change.
type InvoicesResponse struct {
	Envelope
	Facturas []json.RawMessage `json:"facturas"`
}

// ClientDetailsResponse is the GetClientsDetails response. The `datos`
// array contains one client detail object per match; for the typical
// Axentra call it's exactly one entry (idcliente lookup), but the
// endpoint can also return many entries when the caller queries without
// a strict identifier — this proxy supports both.
type ClientDetailsResponse struct {
	Envelope
	Datos []json.RawMessage `json:"datos"`
}

// PingPath / GetInvoicesPath / GetClientsDetailsPath / PaidInvoicePath are
// the four documented Mikrowisp endpoints we know about. Anything else is
// forwarded as a generic pass-through.
//
// Reference: github.com/Mikrowisp/mikrowisp-api (the public Mikrowisp
// API surface). Only GetClientsDetails and GetInvoices receive filter
// treatment; PaidInvoice writes to upstream and we never modify writes.
const (
	PingPath              = "/api/v1/Ping" // unofficial smoke endpoint; treated as pass-through
	GetInvoicesPath       = "/api/v1/GetInvoices"
	GetClientsDetailsPath = "/api/v1/GetClientsDetails"
	PaidInvoicePath       = "/api/v1/PaidInvoice"
)
