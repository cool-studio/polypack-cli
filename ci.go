package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// polypack ci (PolyPack step 57): in a GitHub Actions job with `id-token:
// write`, exchange GitHub's ID token for a Publisher Token of one Pack
// (trusted publishing, ADR-0027), then set up the package manager for it: an
// npm Pack's registry and Token in the user's .npmrc, so a plain `npm
// publish` works; an image's `docker login` (and `helm registry login`). The
// GitHub Action in this repository's action.yml runs it. Unlike npm's own
// exchange, a refusal fails the job with PolyPack's reason.

func cmdCI(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ci", flag.ContinueOnError)
	flags.SetOutput(stderr)
	team := flags.String("team", "", "the Team's slug, as in {team}.pkg.polypack.io")
	registry := flags.String("registry", "", "the npm Registry's URL (default https://{team}.pkg.polypack.io/npm/)")
	oci := flags.String("oci", "", "the OCI Registry's URL (default https://{team}.cr.polypack.io)")
	files := flags.Bool("files", false, "a Files Pack: export POLYPACK_TOKEN for polypack files upload (step 74)")
	pack := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pack, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if pack == "" && flags.NArg() > 0 {
		pack = flags.Arg(0)
	}
	if pack == "" || (*team == "" && *registry == "" && *oci == "") {
		return errors.New("usage: polypack ci <pack> --team SLUG [--files]  (an npm Pack is @scope/name; --files for a Files Pack; anything else is an image)")
	}
	if *registry == "" {
		*registry = "https://" + *team + ".pkg.polypack.io/npm/"
	}
	if *oci == "" {
		*oci = "https://" + *team + ".cr.polypack.io"
	}

	npm := strings.HasPrefix(pack, "@")
	base, err := url.Parse(*registry)
	if !npm && !*files {
		base, err = url.Parse(*oci)
	}
	if err != nil || base.Host == "" {
		return fmt.Errorf("not a registry URL: %v", err)
	}

	var audience, exchange string
	switch {
	case *files:
		// Files (step 74): ours, on the registry host, as on the OCI host.
		audience = "files:" + base.Hostname()
		exchange = base.Scheme + "://" + base.Host + "/-/oidc/token/exchange/pack/" + url.PathEscape(pack)
	case npm:
		if !strings.Contains(pack, "/") {
			return fmt.Errorf("%q is not @scope/name", pack)
		}
		audience = "npm:" + base.Hostname()
		exchange = base.Scheme + "://" + base.Host + "/-/npm/v1/oidc/token/exchange/package/" + strings.Replace(pack, "/", "%2f", 1)
	default:
		audience = "oci:" + base.Hostname()
		exchange = base.Scheme + "://" + base.Host + "/-/oidc/token/exchange/pack/" + pack
	}

	idToken, err := githubIDToken(audience)
	if err != nil {
		return err
	}
	token, expires, err := exchangeIDToken(exchange, idToken)
	if err != nil {
		return err
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Fprintf(stdout, "::add-mask::%s\n", token)
	}

	if *files {
		// The job's later steps read it (polypack files upload), masked above.
		if env := os.Getenv("GITHUB_ENV"); env != "" {
			f, err := os.OpenFile(env, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				return fmt.Errorf("could not write GITHUB_ENV: %w", err)
			}
			_, err = fmt.Fprintf(f, "POLYPACK_TOKEN=%s\n", token)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fmt.Errorf("could not write GITHUB_ENV: %w", err)
			}
		}
		fmt.Fprintf(stdout, "polypack files upload can publish %s until %s (POLYPACK_TOKEN).\n", pack, expires)
		return nil
	}

	if npm {
		path, err := npmrcPath()
		if err != nil {
			return err
		}
		scope := pack[:strings.Index(pack, "/")]
		lines := []string{scope + ":registry=" + *registry, npmAuthKey(*registry) + ":_authToken=" + token}
		if err := editFile(path, func(content string) string { return upsertBlock(content, "ci", lines) }); err != nil {
			return fmt.Errorf("could not update %s: %w", path, err)
		}
		fmt.Fprintf(stdout, "npm can publish %s to %s until %s (%s).\n", pack, *registry, expires, path)
		return nil
	}

	if ran, err := dockerLogin(base.Host, token); err != nil {
		return err
	} else if ran {
		fmt.Fprintf(stdout, "docker can push %s/%s until %s.\n", base.Host, pack, expires)
	}
	if ran, err := helmLogin(base.Host, token); err != nil {
		return err
	} else if ran {
		fmt.Fprintf(stdout, "helm can push to oci://%s/%s until %s.\n", base.Host, pack, expires)
	}
	return nil
}

// githubIDToken asks GitHub Actions for an ID token for this audience, or
// takes POLYPACK_ID_TOKEN (another CI, or a test).
func githubIDToken(audience string) (string, error) {
	if token := os.Getenv("POLYPACK_ID_TOKEN"); token != "" {
		return token, nil
	}
	requestURL, requestToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if requestURL == "" || requestToken == "" {
		return "", errors.New("no GitHub ID token: run this in GitHub Actions, in a job with `permissions: id-token: write`")
	}
	separator := "&"
	if !strings.Contains(requestURL, "?") {
		separator = "?"
	}
	req, err := http.NewRequest(http.MethodGet, requestURL+separator+"audience="+url.QueryEscape(audience), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || body.Value == "" {
		return "", fmt.Errorf("GitHub gave no ID token (%d)", resp.StatusCode)
	}
	return body.Value, nil
}

// exchangeIDToken trades the ID token for a Publisher Token at PolyPack.
func exchangeIDToken(exchange, idToken string) (string, string, error) {
	req, err := http.NewRequest(http.MethodPost, exchange, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+idToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Message   string `json:"message"`
	}
	_ = json.Unmarshal(data, &body)
	if resp.StatusCode != http.StatusCreated || body.Token == "" {
		reason := body.Message
		if reason == "" {
			reason = strings.TrimSpace(string(data))
		}
		return "", "", fmt.Errorf("PolyPack refused the exchange (%d): %s", resp.StatusCode, reason)
	}
	return body.Token, body.ExpiresAt, nil
}

// helmLogin logs helm in to the OCI host, when helm is installed.
func helmLogin(host, token string) (bool, error) {
	path, err := exec.LookPath("helm")
	if err != nil {
		return false, nil
	}
	cmd := exec.Command(path, "registry", "login", host, "--username", "token", "--password-stdin")
	cmd.Stdin = strings.NewReader(token)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return true, fmt.Errorf("helm registry login %s failed: %s", host, strings.TrimSpace(out.String()))
	}
	return true, nil
}
