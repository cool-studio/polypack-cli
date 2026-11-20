package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// Team is what `polypack login` received for one Team: the Token and where
// its registries are.
type Team struct {
	Name      string   `json:"name"`
	Email     string   `json:"email"`
	Host      string   `json:"host"`
	Token     string   `json:"token"`
	ExpiresAt string   `json:"expires_at"`
	NPM       string   `json:"npm"`
	NPMScopes []string `json:"npm_scopes"`
	Composer  string   `json:"composer"`
	OCIHost   string   `json:"oci_host,omitempty"`
}

// Config is the CLI's file, one entry per Team slug. It holds Tokens, so it
// is written 0600.
type Config struct {
	Teams map[string]Team `json:"teams"`
}

func configPath() (string, error) {
	if path := os.Getenv("POLYPACK_CONFIG"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "polypack", "config.json"), nil
}

func loadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	config := &Config{Teams: map[string]Team{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, config); err != nil {
		return nil, err
	}
	if config.Teams == nil {
		config.Teams = map[string]Team{}
	}
	return config, nil
}

func (c *Config) save() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// slugs returns the signed-in Teams, in order.
func (c *Config) slugs() []string {
	slugs := make([]string, 0, len(c.Teams))
	for slug := range c.Teams {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// writeFileAtomic replaces a file in one step, so a crash never leaves half a
// config behind.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".polypack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
