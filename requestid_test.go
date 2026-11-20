package traefik_plugin_request_id

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func newMiddleware(t *testing.T, next http.HandlerFunc) http.Handler {
	t.Helper()
	h, err := New(context.Background(), next, CreateConfig(), "request-id")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func checkID(t *testing.T, id string) {
	t.Helper()
	if len(id) != 32 {
		t.Errorf("expected 32-character identifier, got %q", id)
		return
	}
	if _, err := time.Parse("20060102150405", id[:14]); err != nil {
		t.Errorf("invalid date and time prefix in %q: %v", id, err)
	}
	b, err := hex.DecodeString(id[14:])
	if err != nil || len(b) != 9 || id[14:] != strings.ToUpper(id[14:]) {
		t.Errorf("expected 18 uppercase hexadecimal random characters, got %q", id[14:])
	}
}

func TestNewIDTimestamp(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Second)
	id, err := newID()
	after := time.Now().UTC().Truncate(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	checkID(t, id)
	if len(id) != 32 {
		return
	}
	stamp, err := time.Parse("20060102150405", id[:14])
	if err != nil {
		t.Fatal(err)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Errorf("ID timestamp %s is outside UTC generation interval [%s, %s]", stamp, before, after)
	}
}

func TestClientHeadersAreReplaced(t *testing.T) {
	for _, values := range [][]string{
		nil, {""}, {"client"}, {"one", "two"}, {"bad\r\nvalue"},
		{"20250101192959ABCDEF1234567890AB"},
	} {
		t.Run(fmt.Sprint(values), func(t *testing.T) {
			var id string
			h := newMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
				id = r.Header.Get("X-Request-ID")
				checkID(t, id)
				if slices.Contains(values, id) {
					t.Errorf("client ID was reused: %q", id)
				}
				if got := r.Header.Values("X-Request-ID"); len(got) != 1 {
					t.Errorf("backend IDs = %v", got)
				}
				for key := range r.Header {
					if strings.EqualFold(key, "X-Request-ID") && key != "X-Request-Id" {
						t.Errorf("noncanonical client header survived: %s", key)
					}
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if string(body) != "upload" ||
					r.URL.RequestURI() != "/dav/file?key=value" ||
					r.Header.Get("Authorization") != "Bearer test" {
					t.Error("request payload or unrelated headers changed")
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("response"))
			})
			r := httptest.NewRequest(http.MethodPut, "/dav/file?key=value", strings.NewReader("upload"))
			r.Header["X-Request-Id"] = values
			r.Header["x-request-id"] = []string{"forged"}
			r.Header.Set("Authorization", "Bearer test")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := w.Result().Header.Values("X-Request-ID"); len(got) != 1 || got[0] != id {
				t.Errorf("response IDs = %v, backend ID = %q", got, id)
			}
			if w.Code != http.StatusCreated || w.Body.String() != "response" {
				t.Fatalf("response changed: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestResponseCommitPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"explicit", http.StatusAccepted, ""},
		{"implicit", http.StatusOK, "body"},
		{"empty", http.StatusOK, ""},
		{"flush", http.StatusOK, ""},
		{"error", http.StatusForbidden, "denied\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id string
			h := newMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
				id = r.Header.Get("X-Request-ID")
				switch tc.name {
				case "explicit":
					w.WriteHeader(http.StatusAccepted)
				case "implicit":
					_, _ = w.Write([]byte("body"))
				case "flush":
					w.(http.Flusher).Flush()
				case "error":
					http.Error(w, "denied", http.StatusForbidden)
				}
			})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if got := w.Result().Header.Values("X-Request-ID"); len(got) != 1 || got[0] != id {
				t.Errorf("response IDs %q != single request ID %q", got, id)
			}
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Errorf("response = %d %q, want %d %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
			if tc.name == "flush" && !w.Flushed {
				t.Error("response was not flushed")
			}
		})
	}
}

func TestIndependentConcurrentRequests(t *testing.T) {
	var seen sync.Map
	h := newMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		checkID(t, id)
		if _, loaded := seen.LoadOrStore(id, true); loaded {
			t.Errorf("independent requests reused %q", id)
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodHead, "/health", nil)
			r.Header.Set("X-Request-ID", "same-client-id")
			h.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
}

func TestBackendRetriesKeepID(t *testing.T) {
	var ids []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("X-Request-ID"))
		w.WriteHeader(http.StatusBadGateway)
	})
	retry := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 3; i++ {
			backend.ServeHTTP(httptest.NewRecorder(), r.Clone(r.Context()))
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	w := httptest.NewRecorder()
	newMiddleware(t, retry).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if len(ids) != 3 ||
		ids[0] != ids[1] ||
		ids[0] != ids[2] ||
		ids[0] != w.Header().Get("X-Request-ID") {
		t.Fatalf("retry IDs changed: %v", ids)
	}
}

func newProxyServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	frontend := httptest.NewServer(newMiddleware(t, proxy.ServeHTTP))
	t.Cleanup(frontend.Close)
	frontend.Client().Timeout = 5 * time.Second
	return frontend
}

func TestReverseProxy(t *testing.T) {
	for _, mode := range []string{"no_backend_id", "echoed_id", "different_id"} {
		t.Run(mode, func(t *testing.T) {
			ids := make(chan string, 1)
			frontend := newProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				id := r.Header.Get("X-Request-ID")
				ids <- id
				switch mode {
				case "echoed_id":
					w.Header().Set("X-Request-ID", id)
				case "different_id":
					w.Header().Set("X-Request-ID", "backend-id")
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "partial content")
			})
			req, err := http.NewRequest(http.MethodGet, frontend.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Request-ID", "forged")
			resp, err := frontend.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			id := backendID(t, ids)
			checkID(t, id)
			want := []string{id}
			switch mode {
			case "echoed_id":
				want = append(want, id)
			case "different_id":
				want = append(want, "backend-id")
			}
			// Backend response headers are appended: document the limitation,
			// while requiring exactly one ID in the normal deployment case.
			if got := resp.Header.Values("X-Request-ID"); !slices.Equal(got, want) {
				t.Errorf("response IDs = %q, want %q", got, want)
			}
			if resp.StatusCode != http.StatusPartialContent || string(body) != "partial content" {
				t.Fatalf("proxy response mismatch: %d %q", resp.StatusCode, body)
			}
		})
	}
}

func backendID(t *testing.T, ids <-chan string) string {
	t.Helper()
	select {
	case id := <-ids:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not receive the request")
		return ""
	}
}

func TestReverseProxyEarlyHints(t *testing.T) {
	ids := make(chan string, 1)
	frontend := newProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		ids <- r.Header.Get("X-Request-ID")
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		_, _ = io.WriteString(w, "final response")
	})
	var hints []http.Header
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints {
			hints = append(hints, http.Header(h).Clone())
		}
		return nil
	}}
	ctx := httptrace.WithClientTrace(context.Background(), trace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, frontend.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := frontend.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	id := backendID(t, ids)
	checkID(t, id)
	if len(hints) != 1 {
		t.Fatalf("Early Hints count = %d, want 1", len(hints))
	}
	if got := hints[0].Values("X-Request-ID"); !slices.Equal(got, []string{id}) {
		t.Errorf("Early Hints IDs = %q, want %q", got, id)
	}
	if got := hints[0].Get("Link"); got != "</app.css>; rel=preload" {
		t.Errorf("Early Hints Link = %q", got)
	}
	// Known limitation: the proxy clears the pre-set ID after forwarding 103.
	if got := resp.Header.Values("X-Request-ID"); len(got) != 0 {
		t.Errorf("expected documented missing final ID, got %q", got)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "final response" {
		t.Fatalf("final response mismatch: %d %q", resp.StatusCode, body)
	}
}

func TestReverseProxyUpgrade(t *testing.T) {
	ids := make(chan string, 1)
	frontend := newProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
		ids <- r.Header.Get("X-Request-ID")
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		_, err = io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: request-id-test\r\n\r\nready")
		if err != nil {
			t.Error(err)
			return
		}
		if err := rw.Flush(); err != nil {
			t.Error(err)
			return
		}
		// Exercise the bidirectional tunnel without a WebSocket dependency.
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err != nil {
			t.Error(err)
			return
		}
		if _, err := conn.Write(payload); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Client.Timeout wraps Body without preserving io.Writer. The request
	// context and backend connection deadline bound this upgrade test instead.
	frontend.Client().Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, frontend.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "request-id-test")
	req.Header.Set("X-Request-ID", "forged")
	resp, err := frontend.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "request-id-test" {
		t.Fatalf("upgrade failed: %d %v", resp.StatusCode, resp.Header)
	}
	id := backendID(t, ids)
	checkID(t, id)
	if got := resp.Header.Values("X-Request-ID"); !slices.Equal(got, []string{id}) {
		t.Errorf("upgrade response IDs = %q, want %q", got, id)
	}
	tunnel, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("upgraded response body does not support writing")
	}
	ready := make([]byte, 5)
	if _, err := io.ReadFull(tunnel, ready); err != nil || string(ready) != "ready" {
		t.Fatalf("tunnel greeting = %q, err = %v", ready, err)
	}
	if _, err := io.WriteString(tunnel, "ping"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(tunnel, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("tunnel echo = %q, err = %v", echo, err)
	}
}

func TestConfiguration(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, name := range []string{
		"Host",
		"Content-Length",
		"X-",
		"X-ID\r\nInjected",
		"X-ID:bad",
		"X-ID bad",
		"X-ID/Bad",
		"X-!#$%&'*+-.^_`|~09Az",
		"X-用户",
	} {
		if _, err := New(context.Background(), next, &Config{HeaderName: name}, "test"); err == nil {
			t.Errorf("accepted invalid header name %q", name)
		}
	}
	if _, err := New(context.Background(), next, nil, "test"); err == nil {
		t.Error("accepted nil config")
	}
	if _, err := New(context.Background(), nil, CreateConfig(), "test"); err == nil {
		t.Error("accepted nil next handler")
	}
	for _, name := range []string{"", "x-correlation-id", "X-ID-09Az"} {
		cfg := &Config{HeaderName: name}
		h, err := New(context.Background(), next, cfg, "test")
		if err != nil {
			t.Fatal(err)
		}
		// Configuration reloads must not mutate an existing instance.
		cfg.HeaderName = "X-Changed"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		header := name
		if header == "" {
			header = "X-Request-ID"
		}
		checkID(t, w.Header().Get(header))
	}
}

// This tests the middleware's error handling, not crypto/rand failure behavior.
func TestGenerationFailure(t *testing.T) {
	called := false
	h := newMiddleware(t, func(http.ResponseWriter, *http.Request) {
		called = true
	}).(*middleware)
	h.generate = func() (string, error) { return "partial-id", errors.New("generation failed") }
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "forged")
	h.ServeHTTP(w, r)
	if called ||
		w.Code != http.StatusInternalServerError ||
		len(w.Header().Values("X-Request-ID")) != 0 ||
		w.Body.String() != "request ID generation failed\n" {
		t.Fatalf("generation failure forwarded request: called=%v status=%d", called, w.Code)
	}
}

func TestCustomHeaderReplacement(t *testing.T) {
	const header = "X-Correlation-ID"
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header["X-Correlation-Id"] = []string{"client-one", "client-two"}
	r.Header["x-correlation-id"] = []string{"client-lowercase"}
	r.Header.Set("X-Request-ID", "unrelated-request-id")
	w := httptest.NewRecorder()
	w.Header()["X-Correlation-Id"] = []string{"old-response"}
	w.Header()["x-correlation-id"] = []string{"old-lowercase-response"}
	w.Header().Set("X-Unrelated", "preserved")
	var id string
	h, err := New(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id = r.Header.Get(header)
		checkID(t, id)
		if got := r.Header.Values(header); !slices.Equal(got, []string{id}) {
			t.Errorf("custom request IDs = %q", got)
		}
		if got := r.Header.Get("X-Request-ID"); got != "unrelated-request-id" {
			t.Errorf("unrelated request ID changed: %q", got)
		}
	}), &Config{HeaderName: "x-correlation-id"}, "custom")
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)
	for _, headers := range []http.Header{r.Header, w.Result().Header} {
		if got := headers.Values(header); !slices.Equal(got, []string{id}) {
			t.Errorf("custom IDs = %q, want %q", got, id)
		}
		for key := range headers {
			if strings.EqualFold(key, header) && key != http.CanonicalHeaderKey(header) {
				t.Errorf("noncanonical custom header survived: %q", key)
			}
		}
	}
	if got := w.Result().Header.Get("X-Unrelated"); got != "preserved" {
		t.Errorf("unrelated response header changed: %q", got)
	}
}

func TestOriginalWriterAndNilHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header = nil
	h := newMiddleware(t, func(gotWriter http.ResponseWriter, gotRequest *http.Request) {
		if gotWriter != w || gotRequest != r {
			t.Fatal("original HTTP objects were wrapped or replaced")
		}
		checkID(t, gotRequest.Header.Get("X-Request-ID"))
		if gotWriter.Header().Get("X-Request-ID") != gotRequest.Header.Get("X-Request-ID") {
			t.Fatal("request and response IDs differ")
		}
	})
	h.ServeHTTP(w, r)
}
