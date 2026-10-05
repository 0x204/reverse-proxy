package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// loopHeader carries the ID of every instance a request has passed through,
// so a backend that points back at this proxy is caught on the first hop.
const loopHeader = "X-Proxy-Id"

type proxy struct {
	reverse *httputil.ReverseProxy
	cors    corsPolicy
	id      string
}

func newProxy(backend *url.URL, cors corsPolicy, transport http.RoundTripper) *proxy {
	p := &proxy{
		cors: cors,
		id:   fmt.Sprintf("%016x", rand.Uint64()),
	}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backend)
			// Rewrite has already removed the client's X-Forwarded-* headers;
			// this proxy is the edge, so they could only be spoofed.
			pr.SetXForwarded()
			clientIP, _, _ := net.SplitHostPort(pr.In.RemoteAddr)
			pr.Out.Header.Set("X-Real-Ip", clientIP)
			pr.Out.Header.Add(loopHeader, p.id)
		},
		ModifyResponse: func(res *http.Response) error {
			p.cors.allow(res.Header, res.Request.Header.Get("Origin"))
			return nil
		},
		ErrorHandler: p.handleError,
		Transport:    transport,
	}
	return p
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case !strings.HasPrefix(r.RequestURI, "/"):
		// Absolute-form and CONNECT targets are meant for forward proxies.
		http.Error(w, "invalid request target", http.StatusBadRequest)
	case p.looped(r):
		http.Error(w, "proxy loop detected", http.StatusLoopDetected)
	case r.ContentLength > maxBodySize:
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
	case p.cors.enabled() && isPreflight(r):
		p.cors.preflight(w, r)
	default:
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		p.reverse.ServeHTTP(w, r)
	}
}

func (p *proxy) looped(r *http.Request) bool {
	for _, v := range r.Header.Values(loopHeader) {
		if strings.Contains(v, p.id) {
			return true
		}
	}
	return false
}

// handleError is called when the backend could not be reached or failed
// before sending response headers, so nothing has been written to w yet.
func (p *proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// The error names the backend's address, so it is logged, not returned.
	log.Printf("proxy %s %s: %v", r.Method, r.RequestURI, err)
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// newTransport returns the transport used to reach the backend. A backend
// connection is closed once it has been silent for idle, which catches a hung
// backend without cutting off a long response that is still streaming.
func newTransport(idle time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &http.Transport{
		// Proxy is left nil: the backend is always dialed directly, even if
		// HTTP_PROXY is set.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &idleConn{Conn: conn, timeout: idle}, nil
		},
		MaxIdleConnsPerHost: 256,
		// Retire pooled connections well before their idle deadline, so a
		// request is never sent on one that is about to time out.
		IdleConnTimeout:       idle / 2,
		TLSHandshakeTimeout:   dialTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

// idleConn moves its deadline forward on every read and write, so it times out
// only after it has been silent for timeout. A write also extends the read
// deadline, because the transport starts waiting for a response before the
// request is sent.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// writeTimeoutListener gives every write to a client timeout to complete.
// http.Server.WriteTimeout would cap the whole response instead, cutting off
// downloads that are still making progress. Reads are left alone: http.Server
// reads in the background while a handler runs, and a deadline there would
// cancel the request.
type writeTimeoutListener struct {
	net.Listener
	timeout time.Duration
}

func (l writeTimeoutListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &writeTimeoutConn{Conn: conn, timeout: l.timeout}, nil
}

type writeTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *writeTimeoutConn) Write(b []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// CloseWrite lets http.Server half-close the connection before closing it, so
// an early error response is not lost to a TCP reset.
func (c *writeTimeoutConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
