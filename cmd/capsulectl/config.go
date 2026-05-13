package main

import (
	"fmt"
	"os"
	"path/filepath"

	sigsyaml "sigs.k8s.io/yaml"
)

// Context is one capsule capsulectl knows how to reach: where it lives,
// what TLS fingerprint to pin, what capsule_id to expect (JWT aud), and
// where the operator's signing key sits on disk.
type Context struct {
	Addr           string `json:"addr" yaml:"addr"`
	CapsuleID      string `json:"capsule_id" yaml:"capsule_id"`
	TLSFingerprint string `json:"tls_fingerprint_sha256" yaml:"tls_fingerprint_sha256"`
	KeyPath        string `json:"key_path" yaml:"key_path"`
}

// Config is the persisted capsulectl context registry. Lives at
// ~/.config/capsule/config.yaml (mode 0600, dir 0700).
type Config struct {
	Current  string             `json:"current" yaml:"current"`
	Contexts map[string]Context `json:"contexts" yaml:"contexts"`
}

func configDir() (string, error) {
	if v := os.Getenv("CAPSULE_CONFIG_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "capsule"), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// loadConfig reads the config from disk. A missing file is not an
// error — it returns an empty Config so callers can layer fresh
// contexts onto it.
func loadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Contexts: map[string]Context{}}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := sigsyaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	return &c, nil
}

// Save writes the config back to disk with mode 0600 (parent dir 0700).
// Atomic via temp + rename so an interrupted save doesn't truncate the
// previous good file.
func (c *Config) Save() error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config.yaml")
	b, err := sigsyaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// Resolve returns the context for a given --capsule argument: either a
// context name (e.g. "lab1") or a host:port string. If the value
// matches a context name it wins; otherwise the contexts are searched
// for a matching addr. If --capsule is empty, the current context is
// returned. Returns a sentinel error so callers can produce a friendly
// "run capsulectl adopt" message.
func (c *Config) Resolve(addr string) (Context, error) {
	target := addr
	if target == "" {
		target = c.Current
	}
	if target == "" {
		return Context{}, errNoContext
	}
	if ctx, ok := c.Contexts[target]; ok {
		return ctx, nil
	}
	for _, ctx := range c.Contexts {
		if ctx.Addr == target {
			return ctx, nil
		}
	}
	return Context{}, errNoContext
}

// errNoContext is returned by Resolve when no enrolled context matches
// the operator's --capsule value. dial() catches it and renders a
// pointer to `capsulectl adopt`.
var errNoContext = fmt.Errorf("no enrolled context")
