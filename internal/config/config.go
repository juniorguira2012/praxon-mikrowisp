// Package config loads the proxy's runtime configuration from a TOML file.
//
// The shape of the config and the role each section plays is documented in
// detail in `config.example.toml` next to the project root. This file
// mirrors that shape as Go structs and adds the validation that runs on
// process start; if validation fails the binary refuses to boot with a
// human-readable error rather than producing surprising behavior at
// runtime.
//
// Configuration is intentionally read once at startup and never reloaded.
// To change rules, the operator restarts the container — that's a
// deliberate design choice: filter rules are an audit-relevant decision and
// "did the rules change at 2:14am" should be answerable from the
// container's restart timestamp, not from a hot-reload trace.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the full root config decoded from TOML. Each sub-struct mirrors
// a [section] in the file. Field tags match the TOML keys exactly (lower
// snake case).
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Upstream UpstreamConfig `toml:"upstream"`
	Logging  LoggingConfig  `toml:"logging"`
	Filter   FilterConfig   `toml:"filter"`
}

// ServerConfig governs how this proxy itself listens for inbound requests.
// The upstream caller (Axentra) points its Mikrowisp base_url at the
// `bind` address of this proxy.
type ServerConfig struct {
	Bind                string `toml:"bind"`
	ReadTimeoutSeconds  int    `toml:"read_timeout_seconds"`
	WriteTimeoutSeconds int    `toml:"write_timeout_seconds"`
}

// UpstreamConfig is the real Mikrowisp install behind the proxy. Every
// request the proxy receives is forwarded to BaseURL with the original
// request body intact (token included).
type UpstreamConfig struct {
	BaseURL            string `toml:"base_url"`
	TimeoutSeconds     int    `toml:"timeout_seconds"`
	InsecureSkipVerify bool   `toml:"insecure_skip_verify"`
}

// LoggingConfig controls log output. The proxy never logs request bodies
// or tokens; this only controls verbosity and format.
type LoggingConfig struct {
	Level string `toml:"level"`
	JSON  bool   `toml:"json"`
}

// FilterConfig holds the customer exclusion rule set plus the verdict
// cache configuration. See ExcludeRule for the per-rule shape.
type FilterConfig struct {
	Exclude []ExcludeRule     `toml:"exclude"`
	Cache   FilterCacheConfig `toml:"cache"`
}

// FilterCacheConfig tunes the in-memory cache that stores the exclusion
// verdict per customer ID. The cache holds verdicts only (a bool plus
// timestamp), never the underlying extras data.
type FilterCacheConfig struct {
	TTLSeconds int `toml:"ttl_seconds"`
	MaxEntries int `toml:"max_entries"`
}

// ExcludeRule is one match condition. A customer is dropped if ANY rule's
// condition is satisfied against that customer's extras.
//
// There are two forms:
//
//   - Value match: { field = "X", value = "Y" }
//     The customer's extras must contain field X and its canonical
//     string form must equal Y exactly. Booleans canonicalize to
//     "true"/"false"; numbers to their numeric string; strings stay as-is.
//
//   - Presence match: { field = "X", present = false }
//     The customer's extras must NOT contain field X (the key is missing
//     entirely). Use this to mark customers as not-yet-categorized.
//
// `value` and `present` are mutually exclusive — supply exactly one. The
// validator rejects rules that supply both or neither.
type ExcludeRule struct {
	Field   string  `toml:"field"`
	Value   *string `toml:"value"`
	Present *bool   `toml:"present"`
}

// Load reads the TOML file at path, validates it, and applies sensible
// defaults for fields the operator left empty. Returns an error pointing
// at the first invalid setting.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("config: empty path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// applyDefaults backfills sensible values for keys the operator omitted.
// Keeping the TOML file minimal is a feature; the operator only writes
// the upstream URL and (optionally) some rules, and gets a working proxy.
func (c *Config) applyDefaults() {
	if c.Server.Bind == "" {
		c.Server.Bind = "0.0.0.0:8080"
	}
	if c.Server.ReadTimeoutSeconds == 0 {
		c.Server.ReadTimeoutSeconds = 60
	}
	if c.Server.WriteTimeoutSeconds == 0 {
		c.Server.WriteTimeoutSeconds = 60
	}
	if c.Upstream.TimeoutSeconds == 0 {
		c.Upstream.TimeoutSeconds = 30
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Filter.Cache.TTLSeconds == 0 {
		c.Filter.Cache.TTLSeconds = 300
	}
	if c.Filter.Cache.MaxEntries == 0 {
		c.Filter.Cache.MaxEntries = 10000
	}
	// Strip any trailing slash on the upstream URL so we can append paths
	// uniformly with `+ "/api/v1/..."`.
	c.Upstream.BaseURL = strings.TrimRight(strings.TrimSpace(c.Upstream.BaseURL), "/")
}

// validate catches misconfigurations at startup so the operator gets a
// clean error in the container logs instead of a runtime surprise on the
// first request.
func (c *Config) validate() error {
	if c.Upstream.BaseURL == "" {
		return fmt.Errorf("upstream.base_url is required")
	}
	if !strings.HasPrefix(c.Upstream.BaseURL, "http://") &&
		!strings.HasPrefix(c.Upstream.BaseURL, "https://") {
		return fmt.Errorf("upstream.base_url must start with http:// or https://")
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug/info/warn/error, got %q", c.Logging.Level)
	}
	for i, r := range c.Filter.Exclude {
		if strings.TrimSpace(r.Field) == "" {
			return fmt.Errorf("filter.exclude[%d].field is required", i)
		}
		hasValue := r.Value != nil
		hasPresent := r.Present != nil
		if hasValue == hasPresent {
			// Both supplied OR neither supplied — both are invalid.
			return fmt.Errorf("filter.exclude[%d] must specify exactly one of `value` or `present`", i)
		}
	}
	return nil
}

// ReadTimeout returns the inbound read timeout as a duration. The TOML
// stores integer seconds; this is the conversion we want at the http.Server
// boundary.
func (c *Config) ReadTimeout() time.Duration {
	return time.Duration(c.Server.ReadTimeoutSeconds) * time.Second
}

// WriteTimeout mirrors ReadTimeout for the outbound side of the inbound
// connection.
func (c *Config) WriteTimeout() time.Duration {
	return time.Duration(c.Server.WriteTimeoutSeconds) * time.Second
}

// UpstreamTimeout is the connect/read timeout the proxy uses when calling
// upstream Mikrowisp.
func (c *Config) UpstreamTimeout() time.Duration {
	return time.Duration(c.Upstream.TimeoutSeconds) * time.Second
}

// CacheTTL is the lifetime of a verdict in the filter cache.
func (c *Config) CacheTTL() time.Duration {
	return time.Duration(c.Filter.Cache.TTLSeconds) * time.Second
}
