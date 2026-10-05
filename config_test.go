package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBackend(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:8080",
		"https://api.internal/v1/?key=1",
	}
	for _, raw := range valid {
		if _, err := parseBackend(raw); err != nil {
			t.Errorf("parseBackend(%q): %v", raw, err)
		}
	}

	invalid := []string{
		"",
		"127.0.0.1:8080",
		"ftp://host",
		"http://",
		"http://host:70000",
		"http://host/#frag",
		"http://user:hunter2@host",
		"http://user:hunter2@host:bad",
	}
	for _, raw := range invalid {
		_, err := parseBackend(raw)
		if err == nil {
			t.Errorf("parseBackend(%q): want an error", raw)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("parseBackend(%q): error leaks the password: %v", raw, err)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	os.WriteFile(path, []byte(`{"backend": "http://10.0.0.2:3000"}`), 0o600)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := config{Backend: "http://10.0.0.2:3000", Listen: ":80"}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}

	malformed := []byte(`{"backend": "http://10.0.0.2:3000",}`)
	os.WriteFile(path, malformed, 0o600)
	if _, err := loadConfig(path); err == nil {
		t.Error("malformed config: want an error")
	}
	if data, _ := os.ReadFile(path); string(data) != string(malformed) {
		t.Error("malformed config was overwritten")
	}
}
