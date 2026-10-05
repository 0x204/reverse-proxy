package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type config struct {
	Backend      string `json:"backend"`
	Listen       string `json:"listen"`
	AllowOrigins string `json:"allow_origins"`
}

// loadConfig reads the config at path. If the file does not exist, it asks
// for a backend on the terminal and writes the file.
func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createConfig(path)
	}
	if err != nil {
		return config{}, err
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	return cfg, nil
}

func createConfig(path string) (config, error) {
	missing := fmt.Errorf("%s not found: copy config.json.example to %s and set \"backend\"", path, path)
	if info, err := os.Stdin.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return config{}, missing
	}

	fmt.Print("Backend URL (e.g. http://127.0.0.1:8080): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if errors.Is(err, io.EOF) {
		// Stdin is /dev/null, as under systemd.
		return config{}, missing
	}
	if err != nil {
		return config{}, fmt.Errorf("read backend: %w", err)
	}
	cfg := config{Backend: strings.TrimSpace(line), Listen: defaultListen}
	if _, err := parseBackend(cfg.Backend); err != nil {
		return config{}, err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return config{}, err
	}
	// The file holds the address of an internal host.
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// parseBackend checks that raw is a URL the proxy can forward to.
func parseBackend(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("backend is not set")
	}
	// Error messages never include raw, which may contain a password.
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("backend: %w", errors.Unwrap(err))
	}
	shown := u.Redacted()

	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("backend %q: scheme must be http or https", shown)
	case u.Host == "":
		return nil, fmt.Errorf("backend %q: missing host", shown)
	case u.User != nil:
		return nil, fmt.Errorf("backend %q: credentials in the URL are not supported", shown)
	case u.Fragment != "":
		return nil, fmt.Errorf("backend %q: fragment is not allowed", shown)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("backend %q: invalid port %s", shown, port)
		}
	}
	return u, nil
}
