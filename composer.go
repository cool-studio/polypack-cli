package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// composerHome is Composer's global directory: COMPOSER_HOME, else what
// Composer itself says, else its defaults.
func composerHome() (string, error) {
	if home := os.Getenv("COMPOSER_HOME"); home != "" {
		return home, nil
	}
	if path, err := exec.LookPath("composer"); err == nil {
		if out, err := exec.Command(path, "config", "--global", "home").Output(); err == nil {
			if home := strings.TrimSpace(string(out)); home != "" {
				return home, nil
			}
		}
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "Composer"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "composer"), nil
	}
	if _, err := os.Stat(filepath.Join(home, ".composer")); err == nil {
		return filepath.Join(home, ".composer"), nil
	}
	return filepath.Join(home, ".config", "composer"), nil
}

// composerAuthHost is how Composer keys auth: the host, with a port if any.
func composerAuthHost(registry string) string {
	if u, err := url.Parse(registry); err == nil && u.Host != "" {
		return u.Host
	}
	return registry
}

// setComposerBearer writes (or, with an empty token, removes) the bearer
// Token for a host in auth.json, keeping everything else in it. It writes the
// file itself rather than running `composer config --auth`, which would put
// the Token on a command line.
func setComposerBearer(authFile, host, token string) error {
	auth := map[string]any{}
	data, err := os.ReadFile(authFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &auth); err != nil {
			return err
		}
	}

	bearer, _ := auth["bearer"].(map[string]any)
	if bearer == nil {
		bearer = map[string]any{}
	}
	if token == "" {
		if _, ok := bearer[host]; !ok {
			return nil
		}
		delete(bearer, host)
	} else {
		bearer[host] = token
	}
	if len(bearer) == 0 {
		delete(auth, "bearer")
	} else {
		auth["bearer"] = bearer
	}

	out, err := json.MarshalIndent(auth, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(authFile), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(authFile, append(out, '\n'), 0o600)
}
