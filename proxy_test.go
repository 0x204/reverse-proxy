package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func startBackend(t *testing.T, h http.HandlerFunc) *url.URL {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

func startProxy(t *testing.T, backend *url.URL, cors corsPolicy, idle time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(newProxy(backend, cors, newTransport(idle)))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// rawRequest sends req as written, bypassing any client-side URL cleanup.
func rawRequest(t *testing.T, addr, req string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(body)
}

func echoRequestURI(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, r.RequestURI)
}

func TestRequestTargetCannotChangeHost(t *testing.T) {
	var otherHits atomic.Int32
	other := startBackend(t, func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) })
	addr := startProxy(t, startBackend(t, echoRequestURI), corsPolicy{}, time.Minute)

	target := "//" + other.Host + "/secret?x=://"
	res, body := rawRequest(t, addr, "GET "+target+" HTTP/1.1\r\nHost: x\r\n\r\n")
	if res.StatusCode != http.StatusOK || body != target {
		t.Errorf("got %d %q, want 200 %q", res.StatusCode, body, target)
	}

	for _, line := range []string{
		"GET http://" + other.Host + "/ HTTP/1.1",
		"CONNECT " + other.Host + " HTTP/1.1",
	} {
		res, _ := rawRequest(t, addr, line+"\r\nHost: x\r\n\r\n")
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", line, res.StatusCode)
		}
	}
	if n := otherHits.Load(); n != 0 {
		t.Errorf("other host received %d requests", n)
	}
}

func TestForwardsPathVerbatim(t *testing.T) {
	backend := startBackend(t, echoRequestURI)
	tests := []struct{ backendPath, target, want string }{
		{"", "/a%2Fb/../c?q=%20", "/a%2Fb/../c?q=%20"},
		{"/api/", "/a%2Fb?q=1", "/api/a%2Fb?q=1"},
		{"/api?key=1", "/x?q=1", "/api/x?key=1&q=1"},
	}
	for _, tt := range tests {
		u, _ := url.Parse(backend.String() + tt.backendPath)
		addr := startProxy(t, u, corsPolicy{}, time.Minute)
		_, body := rawRequest(t, addr, "GET "+tt.target+" HTTP/1.1\r\nHost: x\r\n\r\n")
		if body != tt.want {
			t.Errorf("backend %q, target %q: backend got %q, want %q", tt.backendPath, tt.target, body, tt.want)
		}
	}
}

func TestForwardingHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) { got <- r.Header })
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	rawRequest(t, addr, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
		"X-Forwarded-For: 6.6.6.6\r\nX-Real-Ip: 6.6.6.6\r\nX-Forwarded-Proto: https\r\n\r\n")
	h := <-got
	want := map[string]string{
		"X-Forwarded-For":   "127.0.0.1",
		"X-Real-Ip":         "127.0.0.1",
		"X-Forwarded-Host":  "example.com",
		"X-Forwarded-Proto": "http",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
}

func TestStripsConnectionListedHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header
		w.Header().Set("Connection", "X-Internal")
		w.Header().Set("X-Internal", "secret")
	})
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	res, _ := rawRequest(t, addr, "GET / HTTP/1.1\r\nHost: x\r\nConnection: X-Hop\r\nX-Hop: abc\r\n\r\n")
	if v := (<-got).Get("X-Hop"); v != "" {
		t.Errorf("backend received X-Hop: %q", v)
	}
	if v := res.Header.Get("X-Internal"); v != "" {
		t.Errorf("client received X-Internal: %q", v)
	}
}

func TestBodyLimit(t *testing.T) {
	var hits atomic.Int32
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.Copy(io.Discard, r.Body)
	})
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	res, _ := rawRequest(t, addr, fmt.Sprintf("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", maxBodySize+1))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("declared length: got %d, want 413", res.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("backend received %d requests for a declared oversize body", n)
	}

	chunked := io.LimitReader(zeros{}, maxBodySize+1)
	res2, err := http.Post("http://"+addr, "application/octet-stream", io.NopCloser(chunked))
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked: got %d, want 413", res2.StatusCode)
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func TestPostAfterBackendClosesIdleConnection(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
	}))
	srv.Config.IdleTimeout = 50 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	backend, _ := url.Parse(srv.URL)
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	for i := 0; i < 5; i++ {
		res, err := http.Post("http://"+addr, "text/plain", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i, res.StatusCode)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestIdleTimeout(t *testing.T) {
	const idle = 300 * time.Millisecond
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream":
			for i := 0; i < 6; i++ {
				io.WriteString(w, "chunk\n")
				w.(http.Flusher).Flush()
				time.Sleep(idle / 2)
			}
		case "/stall":
			io.WriteString(w, "partial")
			w.(http.Flusher).Flush()
			time.Sleep(3 * idle)
		case "/silent":
			time.Sleep(3 * idle)
		}
	})
	addr := startProxy(t, backend, corsPolicy{}, idle)

	res, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || strings.Count(string(body), "chunk") != 6 {
		t.Errorf("slow stream: got %q, %v; want 6 chunks", body, err)
	}

	res, err = http.Get("http://" + addr + "/stall")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err == nil {
		t.Error("stalled backend: response ended cleanly, want an aborted body")
	}

	res, err = http.Get("http://" + addr + "/silent")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("silent backend: got %d, want 502", res.StatusCode)
	}
}

func TestTruncatedBackendResponseIsAborted(t *testing.T) {
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := http.NewResponseController(w).Hijack()
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nonly ten b")
		conn.Close()
	})
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	// Depending on buffering, the abort shows up before or after the headers.
	res, err := http.Get("http://" + addr)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Error("truncated response ended cleanly, want an error")
	}
}

func TestUnreachableBackend(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	backend, _ := url.Parse("http://" + ln.Addr().String())
	ln.Close()
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	res, body := rawRequest(t, addr, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if res.StatusCode != http.StatusBadGateway || strings.Contains(body, backend.Host) {
		t.Errorf("got %d %q, want 502 without the backend address", res.StatusCode, body)
	}
}

func TestLoopDetected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self, _ := url.Parse("http://" + ln.Addr().String())
	srv := &http.Server{Handler: newProxy(self, corsPolicy{}, newTransport(time.Minute))}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	res, _ := rawRequest(t, ln.Addr().String(), "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if res.StatusCode != http.StatusLoopDetected {
		t.Errorf("got %d, want 508", res.StatusCode)
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		conn, brw, _ := http.NewResponseController(w).Hijack()
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		line, _ := brw.ReadString('\n')
		io.WriteString(conn, line)
	})
	addr := startProxy(t, backend, corsPolicy{}, time.Minute)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("got %d, want 101", res.StatusCode)
	}
	io.WriteString(conn, "ping\n")
	if line, _ := br.ReadString('\n'); line != "ping\n" {
		t.Errorf("echo: got %q", line)
	}
}

func TestCORS(t *testing.T) {
	backend := startBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://backend.example")
		w.Header().Set("X-Handled-By", "backend")
	})

	get := func(addr, method, origin string, extra ...string) *http.Response {
		req := method + " / HTTP/1.1\r\nHost: x\r\n"
		if origin != "" {
			req += "Origin: " + origin + "\r\n"
		}
		for _, h := range extra {
			req += h + "\r\n"
		}
		res, _ := rawRequest(t, addr, req+"\r\n")
		return res
	}
	preflight := []string{"Access-Control-Request-Method: PUT", "Access-Control-Request-Headers: X-Token"}

	t.Run("disabled", func(t *testing.T) {
		addr := startProxy(t, backend, parseCORS(""), time.Minute)
		res := get(addr, "OPTIONS", "https://a.example", preflight...)
		if res.Header.Get("X-Handled-By") != "backend" {
			t.Error("preflight was not forwarded to the backend")
		}
		if v := res.Header.Get("Access-Control-Allow-Origin"); v != "https://backend.example" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the backend's", v)
		}
	})

	t.Run("any origin", func(t *testing.T) {
		addr := startProxy(t, backend, parseCORS("*"), time.Minute)
		if v := get(addr, "GET", "").Header.Get("Access-Control-Allow-Origin"); v != "*" {
			t.Errorf("without Origin: Access-Control-Allow-Origin = %q, want *", v)
		}
		res := get(addr, "OPTIONS", "https://a.example", preflight...)
		if res.StatusCode != http.StatusNoContent ||
			res.Header.Get("Access-Control-Allow-Methods") != "PUT" ||
			res.Header.Get("Access-Control-Allow-Headers") != "X-Token" {
			t.Errorf("preflight: got %d %v", res.StatusCode, res.Header)
		}
	})

	t.Run("origin list", func(t *testing.T) {
		addr := startProxy(t, backend, parseCORS("https://a.example, https://b.example"), time.Minute)
		tests := []struct{ origin, want string }{
			{"https://b.example", "https://b.example"},
			{"https://evil.example", ""},
			{"", ""},
		}
		for _, tt := range tests {
			res := get(addr, "GET", tt.origin)
			if v := res.Header.Get("Access-Control-Allow-Origin"); v != tt.want {
				t.Errorf("Origin %q: Access-Control-Allow-Origin = %q, want %q", tt.origin, v, tt.want)
			}
			if v := res.Header.Get("Vary"); v != "Origin" {
				t.Errorf("Origin %q: Vary = %q, want Origin", tt.origin, v)
			}
		}
	})
}

func TestServeWaitsForInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "done")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln) }()

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		res, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			got <- result{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		got <- result{string(body), err}
	}()

	<-started
	cancel()
	if r := <-got; r.err != nil || r.body != "done" {
		t.Errorf("in-flight request: got %q, %v", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Errorf("serve: %v", err)
	}
}
