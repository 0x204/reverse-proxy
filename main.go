// Command reverse-proxy forwards every request it receives to a single
// configured backend.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	fiber "github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/valyala/fasthttp"
)

const (
	configPath      = "config.json"
	defaultListen   = ":8080"
	maxBodySize     = 64 << 20 // 64 MiB
	backendTimeout  = 60 * time.Second
	shutdownTimeout = 15 * time.Second
	maxBackendConns = 4096
)

// hopByHopHeaders are connection-scoped and must not be forwarded in either
// direction (RFC 9110 section 7.6.1).
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Config is the on-disk configuration.
type Config struct {
	// Backend is the origin every request is forwarded to, e.g. http://127.0.0.1:8080.
	Backend string `json:"backend"`
	// Listen is the address this proxy binds, e.g. ":8080".
	Listen string `json:"listen"`
	// AllowOrigins optionally adds CORS headers to proxied responses, either "*"
	// or a comma-separated origin list. Empty (the default) forwards the
	// backend's own CORS policy untouched, which is what a reverse proxy should
	// normally do -- a blanket "*" here would override a stricter backend policy
	// and expose every endpoint to any origin's JavaScript.
	AllowOrigins string `json:"allow_origins"`
	// Prefork runs one worker process per CPU. Off by default: it re-executes
	// this program, and the children are started without a stdin, so the
	// interactive first-run prompt cannot work in them.
	Prefork bool `json:"prefork"`
}

// loadConfig reads and validates path. A missing file is filled in
// interactively; a malformed one is an error rather than a silent overwrite, so
// a hand-edited config is never clobbered by a typo.
func loadConfig(path string) (Config, error) {
	var cfg Config

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w (fix or delete the file)", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := promptBackend(&cfg, path); err != nil {
			return cfg, err
		}
		if err := saveConfig(path, cfg); err != nil {
			// Refuse to continue: the answer is not persisted, so any prefork
			// child or later restart would come up without a backend.
			return cfg, err
		}
	default:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	return cfg, nil
}

// promptBackend asks for a backend on stdin. Fiber's prefork children are
// started with only Stdout and Stderr wired up, so a read there returns EOF
// immediately and would yield an empty backend; the same is true under systemd
// or "docker run" without a TTY. Refuse in both cases instead of guessing.
func promptBackend(cfg *Config, path string) error {
	if fiber.IsChild() {
		return fmt.Errorf("%s is missing and a prefork child cannot prompt for it", path)
	}
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is missing and stdin is not a terminal: copy config.json.example to %s and set \"backend\"", path, path)
	}

	fmt.Print("Write backend (example: http://127.0.0.1:8080): ")
	backend, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read backend from stdin: %w", err)
	}
	cfg.Backend = strings.TrimSpace(backend)
	cfg.Listen = defaultListen
	return nil
}

// saveConfig writes cfg to path with owner-only permissions, since it holds the
// address of an internal host.
func saveConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// parseBackend validates the configured backend. Without this an unusable value
// -- the "http://ip:port" placeholder, or a host typed with no scheme -- starts
// a proxy that fails every single request.
func parseBackend(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("backend is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse backend %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("backend %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("backend %q: missing host", raw)
	}
	if _, port, splitErr := net.SplitHostPort(u.Host); splitErr == nil && port != "" {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("backend %q: %q is not a valid port", raw, port)
		}
	}
	// Trim a trailing slash so joining with a request path cannot produce "//".
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// checkNotSelf rejects a backend pointing at our own listen address, which would
// make every request recurse into this process until it runs out of sockets.
func checkNotSelf(backend *url.URL, listen string) error {
	_, listenPort, err := net.SplitHostPort(listen)
	if err != nil || listenPort == "" {
		return nil
	}

	backendHost, backendPort, err := net.SplitHostPort(backend.Host)
	if err != nil {
		backendHost, backendPort = backend.Host, "80"
		if backend.Scheme == "https" {
			backendPort = "443"
		}
	}
	if backendPort != listenPort {
		return nil
	}

	ip := net.ParseIP(backendHost)
	if strings.EqualFold(backendHost, "localhost") || (ip != nil && ip.IsLoopback()) {
		return fmt.Errorf("backend %s is this proxy's own listen address %s: requests would loop forever", backend, listen)
	}
	return nil
}

// applyCORS writes the configured CORS headers.
//
// This has to run *after* the upstream call. fasthttp resets the response header
// when it reads the backend's response (Response.ReadLimitBody ->
// resetSkipHeader, then ResponseHeader.tryRead -> resetSkipNormalize), so
// anything set before the proxy call is silently discarded.
func applyCORS(c *fiber.Ctx, allowOrigins string) {
	if allowOrigins == "" {
		return // forward the backend's own policy untouched
	}
	origin := c.Get("Origin")
	if origin == "" {
		return
	}

	if allowOrigins == "*" {
		c.Set("Access-Control-Allow-Origin", "*")
	} else {
		var matched bool
		for _, candidate := range strings.Split(allowOrigins, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), origin) {
				matched = true
				break
			}
		}
		if !matched {
			return
		}
		c.Set("Access-Control-Allow-Origin", origin)
		c.Vary("Origin")
	}
	c.Set("Access-Control-Allow-Methods", "GET,POST,HEAD,PUT,DELETE,PATCH,OPTIONS")
	// Authorization is included deliberately: omitting it makes the preflight
	// reject every cross-origin bearer-token request.
	c.Set("Access-Control-Allow-Headers", "Origin,Content-Type,Accept,Authorization")
	c.Set("Access-Control-Max-Age", "3600")
}

// newProxyHandler forwards a request to backend.
func newProxyHandler(backend *url.URL, client *fasthttp.Client, allowOrigins string) fiber.Handler {
	backendHost := backend.Host
	backendScheme := backend.Scheme
	backendPrefix := backend.Path // trailing slash already trimmed

	return func(c *fiber.Ctx) error {
		req := c.Request()
		res := c.Response()

		// Only origin-form targets ("/path?query") are forwarded. fasthttp's
		// request-line parser does not enforce this, and any other shape is
		// re-parsed as a host further down: a target of "@evil.host/x" would
		// make the backend address look like URL userinfo and re-point the
		// request at an arbitrary server, and an absolute-form target would
		// replace the host outright.
		target := append([]byte(nil), req.Header.RequestURI()...)
		if len(target) == 0 || target[0] != '/' {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request target")
		}
		if backendPrefix != "" {
			target = append([]byte(backendPrefix), target...)
		}

		// Capture everything derived from the inbound request before rewriting it.
		originalTarget := string(req.Header.RequestURI())
		originalHost := string(req.Header.Host())
		clientIP := c.IP()
		method := c.Method()
		// Derived from the connection, not from c.Protocol(): that consults the
		// client's own X-Forwarded-Proto, which would let a client dictate the
		// value we hand the backend.
		clientScheme := "http"
		if c.Context().IsTLS() {
			clientScheme = "https"
		}

		if method == fiber.MethodOptions && c.Get("Access-Control-Request-Method") != "" && allowOrigins != "" {
			applyCORS(c, allowOrigins)
			return c.SendStatus(fiber.StatusNoContent)
		}

		// The backend host is set separately from the path, so no part of the
		// client-controlled target can influence which host we dial.
		req.Header.SetHost(backendHost)
		req.SetRequestURIBytes(target)
		uri := req.URI() // parses using the host set above
		uri.SetScheme(backendScheme)
		// Forward the path exactly as received. Normalizing it here would decode
		// %2F into a real separator and collapse ".." segments before the backend
		// sees them. Must be set after req.URI(), because parsing calls
		// URI.Reset, which clears this flag.
		uri.DisablePathNormalizing = true

		for _, h := range hopByHopHeaders {
			req.Header.Del(h)
		}

		// Overwrite rather than append: this proxy is the edge, so any inbound
		// X-Forwarded-For is client-supplied and spoofed by definition.
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Real-Ip", clientIP)
		req.Header.Set("X-Forwarded-Host", originalHost)
		req.Header.Set("X-Forwarded-Proto", clientScheme)

		if err := client.Do(req, res); err != nil {
			// Log the detail, do not return it: the raw fasthttp error names the
			// backend's host and port.
			log.Printf("proxy %s %s: %v", method, originalTarget, err)
			return fiber.NewError(fiber.StatusBadGateway, "upstream request failed")
		}

		for _, h := range hopByHopHeaders {
			res.Header.Del(h)
		}
		applyCORS(c, allowOrigins)
		return nil
	}
}

// errorHandler keeps failure detail in the log rather than the response body.
func errorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	message := "internal server error"

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		code, message = fiberErr.Code, fiberErr.Message
	} else {
		log.Printf("%s %s: %v", c.Method(), c.OriginalURL(), err)
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	return c.Status(code).SendString(message)
}

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	backend, err := parseBackend(cfg.Backend)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := checkNotSelf(backend, cfg.Listen); err != nil {
		log.Fatalf("config: %v", err)
	}

	client := &fasthttp.Client{
		ReadTimeout:  backendTimeout,
		WriteTimeout: backendTimeout,
		// All traffic goes to one host, so the default per-host cap of 512 is
		// the ceiling for the entire service.
		MaxConnsPerHost: maxBackendConns,
		// Queue for a free connection instead of failing with ErrNoFreeConns.
		MaxConnWaitTimeout:  5 * time.Second,
		MaxIdleConnDuration: 90 * time.Second,
		// Stream responses through instead of accumulating each one in memory.
		// This, rather than MaxResponseBodySize, is what bounds memory: capping
		// the size here would truncate large downloads from our own origin.
		StreamResponseBody: true,
		// Belt and braces with the per-request flag in the handler.
		DisablePathNormalizing: true,
	}

	app := fiber.New(fiber.Config{
		Prefork: cfg.Prefork,
		// Without these an idle connection is held open forever, so a slowloris
		// client can exhaust the listener with no request ever completing. The
		// read timeout also has to cover a slow client uploading up to
		// BodyLimit, so it is generous rather than tight.
		ReadTimeout:           backendTimeout,
		WriteTimeout:          backendTimeout,
		IdleTimeout:           60 * time.Second,
		BodyLimit:             maxBodySize,
		StreamRequestBody:     true,
		DisableStartupMessage: true,
		ErrorHandler:          errorHandler,
	})

	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(newProxyHandler(backend, client, cfg.AllowOrigins))

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		<-signals
		log.Println("shutting down")
		if err := app.ShutdownWithTimeout(shutdownTimeout); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("starting reverse proxy on %s -> %s", cfg.Listen, backend)
	if err := app.Listen(cfg.Listen); err != nil {
		log.Fatalf("listen on %s: %v", cfg.Listen, err)
	}
}
