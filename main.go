// Command reverse-proxy forwards every request it receives to one backend.
package main

import (
	"bufio"
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	configPath      = "config.json"
	defaultListen   = ":80"
	maxBodySize     = 64 << 20
	idleTimeout     = 60 * time.Second // connections silent for this long are closed
	headerTimeout   = 10 * time.Second
	dialTimeout     = 10 * time.Second
	shutdownTimeout = 15 * time.Second
)

func main() {
	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	backend, err := parseBackend(cfg.Backend)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	proxy := newProxy(backend, parseCORS(cfg.AllowOrigins), newTransport(idleTimeout))
	srv := &http.Server{
		Handler:           logRequests(proxy),
		ReadHeaderTimeout: headerTimeout,
		IdleTimeout:       idleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second one kills the process instead of waiting.
	context.AfterFunc(ctx, stop)

	log.Printf("proxying %s to %s", ln.Addr(), backend)
	if err := serve(ctx, srv, writeTimeoutListener{ln, idleTimeout}); err != nil {
		log.Fatal(err)
	}
}

// serve runs srv until ctx is done, then gives in-flight requests up to
// shutdownTimeout to finish.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

// logRequests writes one access log line per request.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		// Deferred so a response aborted mid-body still gets a line.
		defer func() {
			log.Printf("%s %s %s %d %s", r.RemoteAddr, r.Method, r.RequestURI, sw.status, time.Since(start).Round(time.Millisecond))
		}()
		next.ServeHTTP(sw, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack is only called to switch protocols, e.g. for a WebSocket.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = http.StatusSwitchingProtocols
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Unwrap lets http.ResponseController reach Flush on the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
