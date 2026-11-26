package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The CLI's releases, on PolyPack only (step 77, ADR-0037): every release
// is a Version of the public Files Pack `polypack-cli` on Cool Studio's
// Team. `polypack update` and the notice ask there, and check SHA256SUMS
// against a pinned key (pinned.go). There is no other source: when PolyPack
// cannot vouch for a release, the update is refused and says why. A key the
// CLI does not pin (PolyPack rotated its key) means reinstalling.

// reinstall is the line that installs the current CLI, whatever this one is.
const reinstall = "curl -fsSL https://polypack.io/install.sh | sh"

const defaultFilesURL = "https://cool-studio.pkg.polypack.io/files/polypack-cli"

// filesURL is where the CLI's Files Pack is: POLYPACK_CLI_FILES_URL, for
// tests and mirrors, or the default; `off` (the tests' default) asks nobody.
func filesURL() string {
	switch url := os.Getenv("POLYPACK_CLI_FILES_URL"); url {
	case "":
		return defaultFilesURL
	case "off":
		return ""
	default:
		return strings.TrimRight(url, "/")
	}
}

// sourceClient fetches from PolyPack; public, so no Token is ever sent.
var sourceClient = &http.Client{Timeout: 5 * time.Minute}

func sourceGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	return sourceClient.Do(req)
}

// latestFromPolyPack is the tag (`v1.2.0`) on a channel of polypack-cli's
// index.json, computed by PolyPack (`latest`: the newest stable).
func latestFromPolyPack(ctx context.Context, channel string) (string, error) {
	base := filesURL()
	if base == "" {
		return "", errors.New("updates are turned off (POLYPACK_CLI_FILES_URL=off)")
	}
	resp, err := sourceGet(ctx, base+"/index.json")
	if err != nil {
		return "", errors.New("PolyPack could not be reached for updates; try again later")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PolyPack answered %d for updates; try again later", resp.StatusCode)
	}
	var index struct {
		Channels map[string]string `json:"channels"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&index); err != nil {
		return "", errors.New("PolyPack's list of releases could not be read; try again later")
	}
	version := index.Channels[channel]
	if version == "" {
		return "", errors.New("PolyPack has no release on the " + channel + " channel")
	}
	tag := "v" + version
	if _, ok := parseVersion(tag); !ok {
		return "", errors.New("PolyPack's " + channel + " channel names " + version + ", which is not a release")
	}
	return tag, nil
}

// fromPolyPack downloads a release's binary for this platform from PolyPack
// and checks it: SHA256SUMS signed by a pinned key, then the binary's
// SHA-256 in it. Any failure refuses the update.
func fromPolyPack(ctx context.Context, tag string) ([]byte, error) {
	base := filesURL()
	if base == "" {
		return nil, errors.New("updates are turned off (POLYPACK_CLI_FILES_URL=off)")
	}
	versionURL := base + "/" + strings.TrimPrefix(tag, "v")

	sums, _, err := sourceBytes(ctx, versionURL+"/SHA256SUMS", 1<<20)
	if err != nil {
		return nil, err
	}
	sig, header, err := sourceBytes(ctx, versionURL+"/SHA256SUMS.sig", 4096)
	if err != nil {
		return nil, fmt.Errorf("PolyPack does not sign %s, so this polypack cannot check it; not updating", tag)
	}
	if err := verifyPinned(sums, strings.TrimSpace(string(sig)), header.Get("X-PolyPack-Key-Id")); err != nil {
		return nil, err
	}

	name := assetName()
	expected := checksumFor(sums, name)
	if expected == "" {
		return nil, fmt.Errorf("%s has no %s; not updating", tag, name)
	}
	binary, _, err := sourceBytes(ctx, versionURL+"/"+name, maxDownload)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(binary); hex.EncodeToString(sum[:]) != expected {
		return nil, fmt.Errorf("checksum mismatch for %s %s: it is not what its signed SHA256SUMS says; not updating", name, tag)
	}
	return binary, nil
}

// sourceBytes fetches a file from PolyPack; anything but a 200 is an error
// (a release PolyPack does not have, or an outage).
func sourceBytes(ctx context.Context, url string, limit int64) ([]byte, http.Header, error) {
	resp, err := sourceGet(ctx, url)
	if err != nil {
		return nil, nil, errors.New("PolyPack could not be reached; try again later")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("PolyPack answered %d for %s", resp.StatusCode, url[strings.LastIndex(url, "/")+1:])
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, errors.New("PolyPack's answer was cut short; try again")
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%s from PolyPack is too large", url[strings.LastIndex(url, "/")+1:])
	}
	return data, resp.Header, nil
}

// verifyPinned checks SHA256SUMS's signature with a key this CLI pins. A key
// it does not pin (PolyPack rotated its key) cannot be trusted from here, so
// the way on is reinstalling; a signature that does not verify is refused.
func verifyPinned(sums []byte, signature, keyID string) error {
	key, ok := pinnedKeys[keyID]
	if !ok {
		return fmt.Errorf("PolyPack signs with %s, which this polypack does not know. Reinstall it: %s", keyID, reinstall)
	}
	der, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	public, ok := parsed.(*ecdsa.PublicKey)
	if err != nil || !ok {
		return fmt.Errorf("pinned key %s is not an ECDSA public key", keyID)
	}
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("SHA256SUMS.sig from PolyPack is not base64; not updating")
	}
	digest := sha256.Sum256(sums)
	if !ecdsa.VerifyASN1(public, digest[:], raw) {
		return errors.New("SHA256SUMS from PolyPack does not match its signature by PolyPack's key; not updating")
	}
	return nil
}
