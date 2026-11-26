package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// `polypack files` (step 74, ADR-0036): upload a Version's Files to a Files
// Pack and publish it, and download Files, checked against the Version's
// SHA256SUMS and, where PolyPack signs, its signature.

// filesClient makes the Files requests; uploads can be large and slow. A
// redirect away from the registry (a download, to storage) never carries
// the Token: Go keeps Authorization for the same domain on another port,
// and storage refuses a signed URL that also sends one.
var filesClient = &http.Client{
	Timeout: 30 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	},
}

// At most this many Files upload at once.
const parallelUploads = 4

func cmdFiles(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: polypack files upload <pack>@<version> <file>... [--publish] | polypack files download <pack>[@<version or channel>]")
	}
	switch args[0] {
	case "upload":
		return cmdFilesUpload(args[1:], stdout, stderr)
	case "download":
		return cmdFilesDownload(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown files command %q: upload or download", args[0])
	}
}

// filesTarget is where a Files Pack lives and the Token to send: the
// registry's origin, from --registry, the signed-in Team, or
// https://{team}.pkg.polypack.io; the Token from POLYPACK_TOKEN (CI) or the
// Team's sign-in. An empty Token reads only public Packs.
func filesTarget(team, registry string) (string, string, error) {
	token := os.Getenv("POLYPACK_TOKEN")
	config, err := loadConfig()
	if err != nil {
		return "", "", err
	}

	if team == "" && registry == "" {
		switch slugs := config.slugs(); len(slugs) {
		case 0:
			return "", "", errors.New("not signed in: run polypack login, or name the Team with --team")
		case 1:
			team = slugs[0]
		default:
			return "", "", fmt.Errorf("signed in to several Teams (%s): say which with --team", strings.Join(slugs, ", "))
		}
	}

	signedIn, known := config.Teams[team]
	if registry == "" {
		if known {
			registry = signedIn.NPM
		} else {
			registry = "https://" + team + ".pkg.polypack.io"
		}
	}
	u, err := url.Parse(registry)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("not a registry URL: %q", registry)
	}
	if token == "" && known {
		token = signedIn.Token
	}
	return u.Scheme + "://" + u.Host, token, nil
}

// splitRef is pack@ref; a leading v is dropped from a Version (Git tags).
func splitRef(arg string) (string, string) {
	pack, ref, found := strings.Cut(arg, "@")
	if !found {
		return pack, ""
	}
	if len(ref) > 1 && ref[0] == 'v' && ref[1] >= '0' && ref[1] <= '9' {
		ref = ref[1:]
	}
	return pack, ref
}

// filesRequest sends a JSON (or empty) request with the Token, and returns
// the status and body.
func filesRequest(method, target, token string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := filesClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

// --- upload --------------------------------------------------------------------

type reservation struct {
	ID          string            `json:"id"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	CompleteURL string            `json:"complete_url"`
}

func cmdFilesUpload(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("files upload", flag.ContinueOnError)
	flags.SetOutput(stderr)
	team := flags.String("team", "", "the Team's slug")
	registry := flags.String("registry", "", "the registry's URL (default https://{team}.pkg.polypack.io)")
	publish := flags.Bool("publish", false, "publish the Version once every File is uploaded")
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return errors.New("usage: polypack files upload <pack>@<version> <file>... [--team SLUG] [--publish]")
	}
	pack, version := splitRef(positional[0])
	if version == "" {
		return fmt.Errorf("%q names no Version: use <pack>@<version>, e.g. %s@1.0.0", positional[0], pack)
	}
	paths, err := expandFiles(positional[1:])
	if err != nil {
		return err
	}

	origin, token, err := filesTarget(*team, *registry)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("no Token: run polypack login, or set POLYPACK_TOKEN to a Token that can publish")
	}
	base := origin + "/files/" + url.PathEscape(pack) + "/" + url.PathEscape(version)

	var (
		mu       sync.Mutex
		failures []string
		wg       sync.WaitGroup
		slots    = make(chan struct{}, parallelUploads)
	)
	for _, path := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			size, err := uploadFile(base, token, path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", filepath.Base(path), err))
				return
			}
			fmt.Fprintf(stdout, "Uploaded %s (%s) to %s %s.\n", filepath.Base(path), humanBytes(size), pack, version)
		}(path)
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("%d of %d Files did not upload:\n  %s", len(failures), len(paths), strings.Join(failures, "\n  "))
	}

	if !*publish {
		fmt.Fprintf(stdout, "%s %s is a draft: publish it with --publish, or on the Pack's page.\n", pack, version)
		return nil
	}
	status, data, err := filesRequest(http.MethodPost, base+"/publish", token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("could not publish %s %s (%d): %s", pack, version, status, errorText(data))
	}
	fmt.Fprintf(stdout, "Published %s %s.\n", pack, version)
	return nil
}

// expandFiles takes paths and globs (the GitHub Action passes `dist/*`
// quoted, and Windows shells do not expand), and refuses directories and
// two Files with one name.
func expandFiles(args []string) ([]string, error) {
	var paths []string
	seen := map[string]string{}
	for _, arg := range args {
		matches := []string{arg}
		if strings.ContainsAny(arg, "*?[") {
			found, err := filepath.Glob(arg)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid pattern: %w", arg, err)
			}
			if len(found) == 0 {
				return nil, fmt.Errorf("no file matches %q", arg)
			}
			matches = found
		}
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				if strings.ContainsAny(arg, "*?[") {
					continue
				}
				return nil, fmt.Errorf("%s is a directory: name its files, e.g. %s/*", path, path)
			}
			name := filepath.Base(path)
			if other, ok := seen[name]; ok {
				return nil, fmt.Errorf("%s and %s would both be the File %s", other, path, name)
			}
			seen[name] = path
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("no files to upload")
	}
	return paths, nil
}

// uploadFile reserves, PUTs and completes one File. A name already in the
// draft (a rerun of a failed job), or a failed PUT, is removed from the draft
// and tried once more; a published Version is never touched.
func uploadFile(base, token, path string) (int64, error) {
	sum, size, err := fileSHA256(path)
	if err != nil {
		return 0, err
	}
	name := filepath.Base(path)
	try := func() (bool, error) {
		status, data, err := filesRequest(http.MethodPost, base+"/uploads", token, map[string]any{"name": name, "size": size, "sha256": sum})
		if err != nil {
			return false, err
		}
		if status == http.StatusConflict {
			return true, errors.New(errorText(data))
		}
		if status != http.StatusCreated {
			return false, fmt.Errorf("could not reserve it (%d): %s", status, errorText(data))
		}
		var r reservation
		if err := json.Unmarshal(data, &r); err != nil {
			return false, fmt.Errorf("unexpected answer: %w", err)
		}
		if err := putFile(r, path, size); err != nil {
			return true, err
		}
		status, data, err = filesRequest(http.MethodPost, r.CompleteURL, token, nil)
		if err != nil {
			return false, err
		}
		if status != http.StatusOK {
			return false, fmt.Errorf("could not complete it (%d): %s", status, errorText(data))
		}
		return false, nil
	}

	// A rerun's leftover and a failed PUT can both happen: up to three tries,
	// removing whatever the draft holds under this name between them.
	for attempt := 1; ; attempt++ {
		retry, err := try()
		if err == nil || !retry || attempt == 3 {
			return size, err
		}
		status, _, delErr := filesRequest(http.MethodDelete, base+"/"+url.PathEscape(name), token, nil)
		if delErr != nil || status != http.StatusNoContent {
			return 0, err
		}
	}
}

func putFile(r reservation, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	method := r.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequest(method, r.URL, file)
	if err != nil {
		return err
	}
	req.ContentLength = size
	for key, value := range r.Headers {
		if !strings.EqualFold(key, "Content-Length") {
			req.Header.Set(key, value)
		}
	}
	resp, err := filesClient.Do(req)
	if err != nil {
		return fmt.Errorf("the upload failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the upload failed (%d): %s", resp.StatusCode, errorText(data))
	}
	return nil
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	h := sha256.New()
	size, err := io.Copy(h, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// --- download ------------------------------------------------------------------

type filesIndex struct {
	Name     string            `json:"name"`
	Channels map[string]string `json:"channels"`
	Versions []struct {
		Version string `json:"version"`
		Files   []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	} `json:"versions"`
}

func cmdFilesDownload(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("files download", flag.ContinueOnError)
	flags.SetOutput(stderr)
	team := flags.String("team", "", "the Team's slug")
	registry := flags.String("registry", "", "the registry's URL (default https://{team}.pkg.polypack.io)")
	only := flags.String("file", "", "download only this File")
	out := flags.String("out", ".", "the directory to download to")
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: polypack files download <pack>[@<version or channel>] [--file NAME] [--out DIR] [--team SLUG]")
	}
	pack, ref := splitRef(positional[0])
	if ref == "" {
		ref = "latest"
	}

	origin, token, err := filesTarget(*team, *registry)
	if err != nil {
		return err
	}
	base := origin + "/files/" + url.PathEscape(pack)

	status, data, err := filesRequest(http.MethodGet, base+"/index.json", token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("could not read %s (%d): %s", pack, status, errorText(data))
	}
	var index filesIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("unexpected answer: %w", err)
	}

	version := ref
	if ref[0] < '0' || ref[0] > '9' {
		version = index.Channels[ref]
		if version == "" {
			return fmt.Errorf("%s has no Version on the %s channel that this Token may read", pack, ref)
		}
	}
	var names []string
	for _, v := range index.Versions {
		if v.Version == version {
			for _, f := range v.Files {
				if *only == "" || f.Name == *only {
					names = append(names, f.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		if *only != "" {
			return fmt.Errorf("%s %s has no File named %s", pack, version, *only)
		}
		return fmt.Errorf("%s has no Version %s that this Token may read", pack, version)
	}

	sums, signedBy, err := checkedSums(origin, base+"/"+url.PathEscape(version), token)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		want, ok := sums[name]
		if !ok {
			return fmt.Errorf("%s is not in %s %s's SHA256SUMS", name, pack, version)
		}
		size, err := downloadFile(base+"/"+url.PathEscape(version)+"/"+url.PathEscape(name), token, filepath.Join(*out, name), want)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(stdout, "Downloaded %s (%s) of %s %s to %s.\n", name, humanBytes(size), pack, version, *out)
	}
	if signedBy != "" {
		fmt.Fprintf(stdout, "Checked against SHA256SUMS, signed by PolyPack (%s).\n", signedBy)
	} else {
		fmt.Fprintln(stdout, "Checked against SHA256SUMS. This PolyPack does not sign, so there was no signature to check.")
	}
	return nil
}

// checkedSums fetches a Version's SHA256SUMS and, if PolyPack signs, checks
// its signature with the key it names, from /-/polypack/v1/keys. A signature
// that does not verify is an error, never a warning.
func checkedSums(origin, versionURL, token string) (map[string]string, string, error) {
	status, data, err := filesRequest(http.MethodGet, versionURL+"/SHA256SUMS", token, nil)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("could not read SHA256SUMS (%d): %s", status, errorText(data))
	}
	sums := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) == 2 {
			sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
		}
	}

	req, err := http.NewRequest(http.MethodGet, versionURL+"/SHA256SUMS.sig", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := filesClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	sig, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return sums, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("could not read SHA256SUMS.sig (%d)", resp.StatusCode)
	}
	keyID := resp.Header.Get("X-PolyPack-Key-Id")

	status, keysData, err := filesRequest(http.MethodGet, origin+"/-/polypack/v1/keys", "", nil)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("could not fetch PolyPack's keys (%d)", status)
	}
	var keys struct {
		Keys []signingKey `json:"keys"`
	}
	if err := json.Unmarshal(keysData, &keys); err != nil {
		return nil, "", err
	}
	if err := verifySums(data, strings.TrimSpace(string(sig)), keyID, keys.Keys); err != nil {
		return nil, "", err
	}
	return sums, keyID, nil
}

func verifySums(sums []byte, signature, keyID string, keys []signingKey) error {
	var key *signingKey
	for i := range keys {
		if keys[i].KeyID == keyID {
			key = &keys[i]
		}
	}
	if key == nil {
		return fmt.Errorf("SHA256SUMS is signed with %q, which is not one of this PolyPack's keys", keyID)
	}
	der, err := base64.StdEncoding.DecodeString(key.Key)
	if err != nil {
		return err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	public, ok := parsed.(*ecdsa.PublicKey)
	if err != nil || !ok {
		return fmt.Errorf("key %s is not an ECDSA public key", keyID)
	}
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("SHA256SUMS.sig is not base64")
	}
	digest := sha256.Sum256(sums)
	if !ecdsa.VerifyASN1(public, digest[:], raw) {
		return errors.New("SHA256SUMS's signature does not match: it was not signed by PolyPack, or was changed. Nothing was downloaded")
	}
	return nil
}

// downloadFile streams a File to a temporary file beside its destination,
// checks its SHA-256, and only then gives it its name. The redirect to
// storage drops the Token: Go never sends Authorization to another host.
func downloadFile(fileURL, token, dest, want string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, fileURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := filesClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("could not download it (%d): %s", resp.StatusCode, errorText(data))
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".polypack-download-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return 0, fmt.Errorf("its SHA-256 is %s, but SHA256SUMS says %s: not saved", got, want)
	}
	return size, os.Rename(tmp.Name(), dest)
}

// parseInterspersed parses flags wherever they are among the arguments, so
// `polypack files upload x@1 dist/* --publish` works as typed.
func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
