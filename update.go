package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The CLI's releases, as install.sh downloads them (ADR-0034).
const defaultReleasesURL = "https://github.com/cool-studio/polypack-cli/releases"

// A binary is a few MB; anything far larger is not ours.
const maxDownload = 200 << 20

// How often the notice asks, and how long a command may wait for it.
const (
	updateCheckEvery = 24 * time.Hour
	updateCheckLimit = 2 * time.Second
)

// Test seams, as slowDownStep is for the device poll.
var (
	now              = time.Now
	stderrIsTerminal = isTerminal
	executablePath   = currentExecutable
)

func releasesURL() string {
	if url := os.Getenv("POLYPACK_RELEASES_URL"); url != "" {
		return strings.TrimRight(url, "/")
	}
	return defaultReleasesURL
}

// latestRelease reads the newest release's tag from the redirect that
// releases/latest/download/… answers with, without following it.
func latestRelease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releasesURL()+"/latest/download/SHA256SUMS", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent())
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not find the latest release: %w", err)
	}
	resp.Body.Close()
	location, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("could not find the latest release: HTTP %d", resp.StatusCode)
	}
	parts := strings.Split(location.Path, "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "releases" && parts[i+1] == "download" {
			if _, ok := parseVersion(parts[i+2]); ok {
				return parts[i+2], nil
			}
		}
	}
	return "", fmt.Errorf("could not find the latest release in %s", location)
}

// semver is a release's version: vMAJOR.MINOR.PATCH, and since step 75 an
// optional pre-release (`v0.8.0-beta.1`) for the beta channel.
type semver struct {
	nums [3]int
	pre  []string
}

// parseVersion reads vMAJOR.MINOR.PATCH[-PRERELEASE]. Anything else, a dev
// build or a tag that does not parse, is never offered or replaced.
func parseVersion(v string) (semver, bool) {
	var parsed semver
	if !strings.HasPrefix(v, "v") {
		return parsed, false
	}
	core, pre, hasPre := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		return parsed, false
	}
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 || strconv.Itoa(n) != field {
			return parsed, false
		}
		parsed.nums[i] = n
	}
	if hasPre {
		parsed.pre = strings.Split(pre, ".")
		for _, id := range parsed.pre {
			if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
				return semver{}, false
			}
		}
	}
	return parsed, true
}

// isNewer says whether candidate is a later release than current, by
// SemVer's precedence (a pre-release is below its release). False whenever
// either does not parse.
func isNewer(current, candidate string) bool {
	a, ok := parseVersion(current)
	b, ok2 := parseVersion(candidate)
	if !ok || !ok2 {
		return false
	}
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			return b.nums[i] > a.nums[i]
		}
	}
	switch {
	case len(a.pre) == 0:
		return false // a release is never older than its own pre-release
	case len(b.pre) == 0:
		return true
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if a.pre[i] == b.pre[i] {
			continue
		}
		an, aErr := strconv.Atoi(a.pre[i])
		bn, bErr := strconv.Atoi(b.pre[i])
		switch {
		case aErr == nil && bErr == nil:
			return bn > an
		case aErr == nil:
			return true // numeric identifiers sort below alphanumeric ones
		case bErr == nil:
			return false
		default:
			return b.pre[i] > a.pre[i]
		}
	}
	return len(b.pre) > len(a.pre)
}

func assetName() string {
	name := "polypack-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func currentExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func cmdUpdate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	target := flags.String("version", "", "install this release (vX.Y.Z) instead of the latest")
	channel := flags.String("channel", "", "follow this channel, e.g. beta, from now on (latest: stable releases only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cached, cachePath := loadUpdateCheck()
	if *channel != "" {
		// Remembered, so the notice follows it; the cached answer was another channel's.
		cached = updateCheck{Channel: *channel}
		if *channel == "latest" {
			cached.Channel = ""
		}
		saveUpdateCheck(cachePath, cached)
	}
	if _, ok := parseVersion(version); !ok {
		return errors.New("this polypack was built from source; install a release with install.sh to use update")
	}

	exe, err := executablePath()
	if err != nil {
		return fmt.Errorf("could not find this polypack's file: %w", err)
	}
	_ = os.Remove(exe + ".old") // left by an update on Windows

	tag := *target
	if tag == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var note string
		if tag, note, err = latestTag(ctx, cached.channel()); err != nil {
			return err
		}
		if note != "" {
			fmt.Fprintf(stderr, "%s; asking GitHub.\n", note)
		}
		if !isNewer(version, tag) {
			on := ""
			if cached.channel() != "latest" {
				on = " on the " + cached.channel() + " channel"
			}
			fmt.Fprintf(stdout, "polypack %s is the latest version%s.\n", version, on)
			return nil
		}
	} else if _, ok := parseVersion(tag); !ok {
		return fmt.Errorf("--version %q is not a release such as v0.5.0", tag)
	} else if tag == version {
		fmt.Fprintf(stdout, "polypack %s is already installed.\n", version)
		return nil
	}

	// PolyPack first, checked against the pinned key; GitHub when PolyPack
	// cannot answer for this release (step 75). A bad signature or checksum
	// from PolyPack refuses the update, never falls back.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	binary, err := fromPolyPack(ctx, tag)
	if err != nil && !isFallback(err) {
		return err
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s; downloading %s from GitHub.\n", err, tag)
		if binary, err = fromGitHub(tag); err != nil {
			return err
		}
	}

	if err := replaceExecutable(exe, binary); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot write %s. Run sudo polypack update, or install.sh again", exe)
		}
		return fmt.Errorf("could not replace %s: %w", exe, err)
	}
	fmt.Fprintf(stdout, "polypack updated: %s → %s\n", version, tag)
	return nil
}

// fromGitHub downloads a release's binary for this platform from its GitHub
// Release, checked against the release's SHA256SUMS (step 70).
func fromGitHub(tag string) ([]byte, error) {
	base := releasesURL() + "/download/" + tag + "/"
	name := assetName()
	sums, err := downloadRelease(base + "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	binary, err := downloadRelease(base + name)
	if err != nil {
		return nil, err
	}
	expected := checksumFor(sums, name)
	actual := sha256.Sum256(binary)
	if expected == "" || expected != hex.EncodeToString(actual[:]) {
		return nil, fmt.Errorf("checksum mismatch for %s %s; not updating", name, tag)
	}
	return binary, nil
}

func downloadRelease(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not download %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", url, err)
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("could not download %s: too large", url)
	}
	return data, nil
}

// checksumFor finds a file's line in SHA256SUMS ("<hex>  <name>").
func checksumFor(sums []byte, name string) string {
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// replaceExecutable swaps the binary in one step. Windows cannot overwrite
// a running .exe, but can rename it, so the old one steps aside first.
func replaceExecutable(exe string, binary []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".polypack-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(exe, exe+".old"); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), exe); err != nil {
			_ = os.Rename(exe+".old", exe)
			return err
		}
		return nil
	}
	return os.Rename(tmp.Name(), exe)
}

// updateCheck is update-check.json, beside the config: when the notice last
// asked, and the newest release it heard of.
type updateCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest,omitempty"`
	// The channel `polypack update --channel` last chose (step 75); the
	// notice follows it. Empty is `latest`.
	Channel string `json:"channel,omitempty"`
}

func loadUpdateCheck() (updateCheck, string) {
	var cached updateCheck
	path, err := updateCheckPath()
	if err != nil {
		return cached, ""
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cached)
	}
	return cached, path
}

func saveUpdateCheck(path string, checked updateCheck) {
	if path == "" {
		return
	}
	if data, err := json.Marshal(checked); err == nil && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
}

func (c updateCheck) channel() string {
	if c.Channel == "" {
		return "latest"
	}
	return c.Channel
}

func updateCheckPath() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "update-check.json"), nil
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Commands after which the notice may appear; never ci, update or help.
var noticeCommands = map[string]bool{
	"login": true, "setup": true, "doctor": true, "files": true, "whoami": true, "logout": true, "verify": true, "version": true,
}

func noticeWanted(command string, stderr io.Writer) bool {
	if !noticeCommands[command] {
		return false
	}
	for _, name := range []string{"POLYPACK_NO_UPDATE_CHECK", "CI", "GITHUB_ACTIONS"} {
		if os.Getenv(name) != "" {
			return false
		}
	}
	if _, ok := parseVersion(version); !ok {
		return false
	}
	return stderrIsTerminal(stderr)
}

// startUpdateNotice begins the once-a-day check alongside a command. The
// returned function, called when the command ends, prints the notice if a
// newer release exists, waiting at most until the check's time limit.
func startUpdateNotice(command string, stderr io.Writer) func() {
	if !noticeWanted(command, stderr) {
		return func() {}
	}
	cached, path := loadUpdateCheck()
	if path == "" {
		return func() {}
	}
	show := func(latest string) {
		if isNewer(version, latest) {
			fmt.Fprintf(stderr, "\nA new version of polypack is available: %s → %s\nRun `polypack update` to install it.\n", version, latest)
		}
	}
	if age := now().Sub(cached.CheckedAt); age >= 0 && age < updateCheckEvery {
		return func() { show(cached.Latest) }
	}

	deadline := time.Now().Add(updateCheckLimit)
	result := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		checked := updateCheck{CheckedAt: now(), Latest: cached.Latest, Channel: cached.Channel}
		// PolyPack's channel first, else GitHub's latest (step 75).
		if latest, _, err := latestTag(ctx, cached.channel()); err == nil {
			checked.Latest = latest
		}
		saveUpdateCheck(path, checked)
		result <- checked.Latest
	}()
	return func() {
		timer := time.NewTimer(time.Until(deadline) + 100*time.Millisecond)
		defer timer.Stop()
		select {
		case latest := <-result:
			show(latest)
		case <-timer.C:
		}
	}
}
