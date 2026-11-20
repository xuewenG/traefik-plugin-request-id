// Package traefik_plugin_request_id assigns a fresh identifier to each ingress request.
package traefik_plugin_request_id

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Config is the Traefik middleware configuration.
type Config struct {
	HeaderName string `json:"headerName,omitempty"`
}

// CreateConfig returns the default plugin configuration.
func CreateConfig() *Config {
	return &Config{HeaderName: "X-Request-ID"}
}

// New creates a middleware. Client identifiers are never trusted or reused.
func New(
	_ context.Context,
	next http.Handler,
	config *Config,
	_ string,
) (http.Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("request-id: config is required")
	}
	if next == nil {
		return nil, fmt.Errorf("request-id: next handler is required")
	}
	header := config.HeaderName
	if header == "" {
		header = "X-Request-ID"
	}
	// Require an X- name; callers must avoid headers with existing semantics.
	if !isValidHeaderName(header) {
		return nil, fmt.Errorf("request-id: headerName must be a valid X- extension header")
	}
	return &middleware{
		next:     next,
		header:   http.CanonicalHeaderKey(header),
		generate: newID,
	}, nil
}

type middleware struct {
	next     http.Handler
	header   string
	generate func() (string, error)
}

func newID() (string, error) {
	var entropy [9]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	timestamp := time.Now().UTC().Format("20060102150405")
	randomPart := strings.ToUpper(hex.EncodeToString(entropy[:]))
	return timestamp + randomPart, nil
}

func (m *middleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := m.generate()
	if err != nil {
		// Stop on generation errors instead of reusing a client ID.
		http.Error(w, "request ID generation failed", http.StatusInternalServerError)
		return
	}
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	replaceHeader(r.Header, m.header, id)
	replaceHeader(w.Header(), m.header, id)
	// Keep the original writer: Yaegi wrappers can lose optional interfaces.
	m.next.ServeHTTP(w, r)
}

func isValidHeaderName(s string) bool {
	const allowedChars = "abcdefghijklmnopqrstuvwxyz" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" +
		"-"

	return len(s) > 2 &&
		strings.EqualFold(s[:2], "X-") &&
		strings.TrimLeft(s[2:], allowedChars) == ""
}

// replaceHeader also removes case variants inserted directly into the map.
func replaceHeader(h http.Header, name, value string) {
	for key := range h {
		if strings.EqualFold(key, name) {
			delete(h, key)
		}
	}
	h.Set(name, value)
}
