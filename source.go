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

// The CLI's releases on PolyPack (step 75, ADR-0034 as amended, ADR-0036
// § 9): every release is also a Version of the public Files Pack
// `polypack-cli` on Cool Studio's Team. `polypack update` and the notice ask
// there first, check SHA256SUMS against a pinned key (pinned.go), and fall
// back to GitHub's releases when PolyPack cannot answer for a release, or
// signs with a key this CLI does not pin. A signature or checksum that does
// not match is never a fallback: the update is refused.

const defaultFilesURL = "https://cool-studio.pkg.polypack.io/files/polypack-cli"

// filesURL is where the CLI's Files Pack is: POLYPACK_CLI_FILES_URL, for
// tests and mirrors, or the default; `off` asks GitHub only.
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

// fallback is an answer PolyPack could not give: the update goes to GitHub,
// with this as the note. Any other error refuses the update.
type fallback struct{ reason string }

func (f fallback) Error() string { return f.reason }

func isFallback(err error) bool {
	var f fallback
	return errors.As(err, &f)
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
		return "", fallback{"PolyPack is turned off for updates"}
	}
	resp, err := sourceGet(ctx, base+"/index.json")
	if err != nil {
		return "", fallback{"PolyPack could not be reached"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fallback{fmt.Sprintf("PolyPack answered %d", resp.StatusCode)}
	}
	var index struct {
		Channels map[string]string `json:"channels"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&index); err != nil {
		return "", fallback{"PolyPack's index could not be read"}
	}
	version := index.Channels[channel]
	if version == "" {
		return "", fallback{"PolyPack has no release on the " + channel + " channel"}
	}
	tag := "v" + version
	if _, ok := parseVersion(tag); !ok {
		return "", fallback{"PolyPack's " + channel + " is not a release: " + version}
	}
	return tag, nil
}

// latestTag is the newest release on a channel: from PolyPack, or else from
// GitHub, which knows only its latest stable release.
func latestTag(ctx context.Context, channel string) (string, string, error) {
	tag, err := latestFromPolyPack(ctx, channel)
	if err == nil {
		return tag, "", nil
	}
	note := err.Error()
	tag, err = latestRelease(ctx)
	return tag, note, err
}

// fromPolyPack downloads a release's binary for this platform from PolyPack
// and checks it: SHA256SUMS signed by a pinned key, then the binary's
// SHA-256 in it. A fallback error means GitHub should be asked instead.
func fromPolyPack(ctx context.Context, tag string) ([]byte, error) {
	base := filesURL()
	if base == "" {
		return nil, fallback{"PolyPack is turned off for updates"}
	}
	versionURL := base + "/" + strings.TrimPrefix(tag, "v")

	sums, _, err := sourceBytes(ctx, versionURL+"/SHA256SUMS", 1<<20)
	if err != nil {
		return nil, err
	}
	sig, header, err := sourceBytes(ctx, versionURL+"/SHA256SUMS.sig", 4096)
	if err != nil {
		return nil, fallback{"PolyPack does not sign " + tag}
	}
	if err := verifyPinned(sums, strings.TrimSpace(string(sig)), header.Get("X-PolyPack-Key-Id")); err != nil {
		return nil, err
	}

	name := assetName()
	expected := checksumFor(sums, name)
	if expected == "" {
		return nil, fallback{tag + " on PolyPack has no " + name}
	}
	binary, _, err := sourceBytes(ctx, versionURL+"/"+name, maxDownload)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(binary); hex.EncodeToString(sum[:]) != expected {
		return nil, fmt.Errorf("%s %s from PolyPack is not what its signed SHA256SUMS says; not updating", name, tag)
	}
	return binary, nil
}

// sourceBytes fetches a file from PolyPack; anything but a 200 is a
// fallback (a release PolyPack does not have, or an outage).
func sourceBytes(ctx context.Context, url string, limit int64) ([]byte, http.Header, error) {
	resp, err := sourceGet(ctx, url)
	if err != nil {
		return nil, nil, fallback{"PolyPack could not be reached"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fallback{fmt.Sprintf("PolyPack answered %d for %s", resp.StatusCode, url[strings.LastIndex(url, "/")+1:])}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, fallback{"PolyPack's answer was cut short"}
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%s from PolyPack is too large", url[strings.LastIndex(url, "/")+1:])
	}
	return data, resp.Header, nil
}

// verifyPinned checks SHA256SUMS's signature with a key this CLI pins. A key
// it does not pin (PolyPack rotated its key) is a fallback to GitHub; a
// signature by a pinned key that does not verify is refused outright.
func verifyPinned(sums []byte, signature, keyID string) error {
	key, ok := pinnedKeys[keyID]
	if !ok {
		return fallback{"PolyPack signs with " + keyID + ", which this polypack does not know (a newer release will)"}
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
