// Package proxy contains the HTTP handler logic. This file is the thin
// HTTP client that talks to the real Mikrowisp install on behalf of the
// proxy.
//
// We intentionally do NOT use net/http/httputil.ReverseProxy because we
// need to read the upstream response body, parse it as JSON, mutate the
// payload arrays, and re-emit. ReverseProxy is built for byte-for-byte
// streaming and would fight us at every step.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Upstream is a tiny HTTP client preconfigured for the upstream Mikrowisp
// base URL. Each public method maps 1-to-1 onto a Mikrowisp endpoint we
// need to consult while filtering.
type Upstream struct {
	baseURL string
	client  *http.Client
}

// NewUpstream constructs an Upstream pointed at baseURL with the given
// timeout and TLS posture. Pass insecureSkipVerify=true only when the
// upstream Mikrowisp uses a self-signed cert on a private network you
// trust; never on the public internet.
func NewUpstream(baseURL string, timeout time.Duration, insecureSkipVerify bool) *Upstream {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecureSkipVerify,
		},
		// Modest pool sizing — even a busy ISP poll is ~100 req/min,
		// well below default limits. Explicit values for clarity.
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Upstream{
		baseURL: baseURL,
		client: &http.Client{
			Transport: tr,
			Timeout:   timeout,
		},
	}
}

// Forward sends `body` (a JSON object — the raw inbound POST body from the
// caller) to the upstream Mikrowisp endpoint at `path` and returns the
// raw response body plus the HTTP status. Forward never modifies the body
// going up; the token, limit, and any other caller-supplied fields ride
// through verbatim.
//
// Returned bytes are the upstream JSON response. The caller is responsible
// for envelope inspection and (where applicable) filtering.
func (u *Upstream) Forward(ctx context.Context, path string, body []byte) (statusCode int, response []byte, err error) {
	url := u.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("upstream: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "axentra-mikrowisp-proxy/1.0")

	resp, err := u.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("upstream %s: %w", path, err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("upstream %s: read response: %w", path, err)
	}
	return resp.StatusCode, out, nil
}

// GetClientDetails calls /api/v1/GetClientsDetails for a single client
// id, using the same token the inbound request supplied. The response is
// returned as a generic JSON object (string -> raw JSON), suitable for
// the filter to inspect arbitrary extra fields on.
//
// This is the call the filter cache makes on a miss when it needs to
// decide whether a not-yet-seen customer should be excluded.
func (u *Upstream) GetClientDetails(ctx context.Context, token string, idCliente int) (map[string]json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"token":     token,
		"idcliente": idCliente,
	})
	if err != nil {
		return nil, fmt.Errorf("upstream: marshal GetClientsDetails: %w", err)
	}
	_, raw, err := u.Forward(ctx, "/api/v1/GetClientsDetails", body)
	if err != nil {
		return nil, err
	}
	// Parse the envelope + datos[]; we expect at most one entry for an
	// idcliente lookup. Per Mikrowisp behavior, no match returns an
	// `estado=error` envelope, which we surface as ErrClientNotFound.
	var resp struct {
		Estado  string                       `json:"estado"`
		Mensaje string                       `json:"mensaje"`
		Datos   []map[string]json.RawMessage `json:"datos"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("upstream: parse GetClientsDetails: %w", err)
	}
	if resp.Estado != "exito" || len(resp.Datos) == 0 {
		return nil, ErrClientNotFound
	}
	return resp.Datos[0], nil
}

// ErrClientNotFound is returned by GetClientDetails when upstream reports
// no match for the supplied id. Callers treat this as "the client doesn't
// exist; nothing to filter on" rather than a hard error.
var ErrClientNotFound = fmt.Errorf("upstream: client not found")
