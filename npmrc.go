package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// npmrcPath is the user's own npm config, never a project's: npm's
// userconfig if set, else ~/.npmrc.
func npmrcPath() (string, error) {
	for _, key := range []string{"npm_config_userconfig", "NPM_CONFIG_USERCONFIG"} {
		if path := os.Getenv(key); path != "" {
			return path, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".npmrc"), nil
}

// npmAuthKey is how npm keys a registry's auth: `//host[:port]/npm/`.
func npmAuthKey(registry string) string {
	u, err := url.Parse(registry)
	if err != nil || u.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(registry, "https:"), "http:")
	}
	path := u.Path
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return "//" + u.Host + path
}

func npmBlockLines(team Team) []string {
	lines := make([]string, 0, len(team.NPMScopes)+1)
	for _, scope := range team.NPMScopes {
		lines = append(lines, scope+":registry="+team.NPM)
	}
	return append(lines, npmAuthKey(team.NPM)+":_authToken="+team.Token)
}

func blockMarkers(slug string) (string, string) {
	return "# >>> polypack " + slug, "# <<< polypack " + slug
}

// upsertBlock puts lines between the Team's markers, replacing an earlier
// block in place, or appending one. Nothing outside the block changes.
func upsertBlock(content, slug string, lines []string) string {
	start, end := blockMarkers(slug)
	block := start + "\n" + strings.Join(lines, "\n") + "\n" + end + "\n"

	if before, after, found := cutBlock(content, start, end); found {
		return before + block + after
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + block
}

// removeBlock takes the Team's block out, if there is one.
func removeBlock(content, slug string) string {
	start, end := blockMarkers(slug)
	if before, after, found := cutBlock(content, start, end); found {
		return before + after
	}
	return content
}

func cutBlock(content, start, end string) (string, string, bool) {
	i := strings.Index(content, start+"\n")
	if i < 0 {
		return "", "", false
	}
	j := strings.Index(content[i:], end)
	if j < 0 {
		return "", "", false
	}
	rest := content[i+j+len(end):]
	rest = strings.TrimPrefix(rest, "\n")
	return content[:i], rest, true
}

func editFile(path string, edit func(string) string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated := edit(string(data))
	if updated == string(data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(updated), 0o600)
}
