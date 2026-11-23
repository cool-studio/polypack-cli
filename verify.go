package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provenance (PolyPack step 52, ADR-0028): a Version published through
// trusted publishing carries an in-toto statement, signed by PolyPack in a
// DSSE envelope, of the repository, workflow, commit and run it came from.
// `polypack verify` checks the signature with PolyPack's published key, and
// that the registry serves exactly what the statement names.

type envelope struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
	Signatures  []struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		BuildDefinition struct {
			ExternalParameters struct {
				Workflow struct {
					Ref        string `json:"ref"`
					Repository string `json:"repository"`
					Path       string `json:"path"`
				} `json:"workflow"`
			} `json:"externalParameters"`
			ResolvedDependencies []struct {
				Digest map[string]string `json:"digest"`
			} `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder struct {
				ID string `json:"id"`
			} `json:"builder"`
			Metadata struct {
				InvocationID string `json:"invocationId"`
				StartedOn    string `json:"startedOn"`
			} `json:"metadata"`
		} `json:"runDetails"`
	} `json:"predicate"`
}

type signingKey struct {
	KeyID   string  `json:"keyid"`
	Key     string  `json:"key"`
	Expires *string `json:"expires"`
}

const (
	inTotoPayload = "application/vnd.in-toto+json"
	statementType = "https://in-toto.io/Statement/v1"
	slsaPredicate = "https://slsa.dev/provenance/v1"
)

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || (args[0] != "npm" && args[0] != "oci") {
		return errors.New("usage: polypack verify npm @scope/name@version | polypack verify oci name:tag [--team SLUG]")
	}
	kind, target := args[0], args[1]
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	only := flags.String("team", "", "the Team whose registry to ask")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}

	team, err := verifyTeam(*only)
	if err != nil {
		return err
	}
	registry, err := url.Parse(team.NPM)
	if err != nil || registry.Host == "" {
		return fmt.Errorf("the Team's npm registry %q is not a URL", team.NPM)
	}
	scheme := registry.Scheme

	var origin, provenancePath, subjectName string
	var served func() (map[string]string, error)
	switch kind {
	case "npm":
		at := strings.LastIndex(target, "@")
		if !strings.HasPrefix(target, "@") || at <= 0 || !strings.Contains(target[:at], "/") {
			return fmt.Errorf("%q is not @scope/name@version", target)
		}
		name, ver := target[:at], target[at+1:]
		origin = scheme + "://" + registry.Host
		provenancePath = "/-/polypack/v1/provenance/" + name + "/" + url.PathEscape(ver)
		subjectName = "pkg:npm/" + strings.Replace(name, "@", "%40", 1) + "@" + ver
		served = func() (map[string]string, error) { return npmTarballDigest(team, name, ver) }
	default:
		if team.OCIHost == "" {
			return errors.New("this Team's Plan has no OCI Registry")
		}
		colon := strings.LastIndex(target, ":")
		if colon <= 0 || strings.Contains(target[colon:], "/") {
			return fmt.Errorf("%q is not name:tag", target)
		}
		name, tag := target[:colon], target[colon+1:]
		origin = scheme + "://" + team.OCIHost
		provenancePath = "/-/polypack/v1/provenance/" + name + "/" + tag
		subjectName = hostname(team.OCIHost) + "/" + name
		served = func() (map[string]string, error) { return ociManifestDigest(team, origin, name, tag) }
	}

	resp, data, err := registryRequest(http.MethodGet, origin+provenancePath, team.Token)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("no provenance for %s (%d): %s", target, resp.StatusCode, errorText(data))
	}
	var env envelope
	if err := jsonUnmarshal(data, &env); err != nil {
		return fmt.Errorf("the provenance is not a DSSE envelope: %v", err)
	}

	resp, data, err = registryRequest(http.MethodGet, origin+"/-/polypack/v1/keys", team.Token)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not fetch PolyPack's keys (%d): %s", resp.StatusCode, errorText(data))
	}
	var keys struct {
		Keys []signingKey `json:"keys"`
	}
	if err := jsonUnmarshal(data, &keys); err != nil {
		return err
	}

	st, err := verifyEnvelope(env, keys.Keys)
	if err != nil {
		return err
	}

	digest, err := served()
	if err != nil {
		return err
	}
	if err := checkSubject(st, subjectName, digest); err != nil {
		return err
	}

	b := st.Predicate.BuildDefinition
	commit := ""
	if len(b.ResolvedDependencies) > 0 {
		commit = b.ResolvedDependencies[0].Digest["gitCommit"]
	}
	fmt.Fprintf(stdout, "Verified %s: signed by PolyPack (%s), and the registry serves what was published.\n", target, env.Signatures[0].KeyID)
	fmt.Fprintf(stdout, "  Repository: %s\n", b.ExternalParameters.Workflow.Repository)
	fmt.Fprintf(stdout, "  Commit:     %s (%s)\n", commit, b.ExternalParameters.Workflow.Ref)
	fmt.Fprintf(stdout, "  Workflow:   %s\n", b.ExternalParameters.Workflow.Path)
	fmt.Fprintf(stdout, "  Run:        %s\n", st.Predicate.RunDetails.Metadata.InvocationID)
	fmt.Fprintf(stdout, "  Runner:     %s\n", st.Predicate.RunDetails.Builder.ID)
	return nil
}

// verifyEnvelope checks the envelope's signature over DSSE's pre-authentication
// encoding with the key its key id names, and returns the statement.
func verifyEnvelope(env envelope, keys []signingKey) (*statement, error) {
	if env.PayloadType != inTotoPayload || len(env.Signatures) == 0 {
		return nil, errors.New("the provenance is not a signed in-toto statement")
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, errors.New("the provenance's payload is not base64")
	}
	var st statement
	if err := jsonUnmarshal(payload, &st); err != nil {
		return nil, fmt.Errorf("the provenance's statement is not JSON: %v", err)
	}

	sig := env.Signatures[0]
	var key *signingKey
	for i := range keys {
		if keys[i].KeyID == sig.KeyID {
			key = &keys[i]
		}
	}
	if key == nil {
		return nil, fmt.Errorf("no PolyPack key %s: it signed nothing this registry vouches for", sig.KeyID)
	}
	der, err := base64.StdEncoding.DecodeString(key.Key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	public, ok := parsed.(*ecdsa.PublicKey)
	if err != nil || !ok {
		return nil, fmt.Errorf("key %s is not an ECDSA public key", sig.KeyID)
	}
	signature, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return nil, errors.New("the signature is not base64")
	}
	sum := sha256.Sum256(pae(env.PayloadType, payload))
	if !ecdsa.VerifyASN1(public, sum[:], signature) {
		return nil, errors.New("the signature does not match: this provenance was not signed by PolyPack, or was changed")
	}

	// A retired key verifies only what it signed before it was retired.
	if key.Expires != nil {
		expires, err1 := time.Parse(time.RFC3339, *key.Expires)
		signed, err2 := time.Parse(time.RFC3339, st.Predicate.RunDetails.Metadata.StartedOn)
		if err1 != nil || err2 != nil || !signed.Before(expires) {
			return nil, fmt.Errorf("key %s was retired on %s, before this was signed", key.KeyID, *key.Expires)
		}
	}

	if st.Type != statementType || st.PredicateType != slsaPredicate || len(st.Subject) != 1 {
		return nil, errors.New("the statement is not SLSA provenance for one subject")
	}
	return &st, nil
}

// checkSubject compares the statement's subject with what the registry serves.
func checkSubject(st *statement, name string, served map[string]string) error {
	subject := st.Subject[0]
	if subject.Name != name {
		return fmt.Errorf("the provenance is for %s, not %s", subject.Name, name)
	}
	for algorithm, value := range served {
		if subject.Digest[algorithm] != value {
			return fmt.Errorf("the registry serves %s %s, but the provenance names %s: it is not what was published", algorithm, value, subject.Digest[algorithm])
		}
	}
	return nil
}

// pae is DSSE's pre-authentication encoding.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}

func npmTarballDigest(team Team, name, ver string) (map[string]string, error) {
	resp, data, err := registryRequest(http.MethodGet, strings.TrimSuffix(team.NPM, "/")+"/"+strings.Replace(name, "/", "%2f", 1), team.Token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not read %s (%d): %s", name, resp.StatusCode, errorText(data))
	}
	var packument struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := jsonUnmarshal(data, &packument); err != nil {
		return nil, err
	}
	v, ok := packument.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("%s has no version %s", name, ver)
	}
	sum, err := download(v.Dist.Tarball, team.Token, sha512.New(), nil)
	if err != nil {
		return nil, err
	}
	return map[string]string{"sha512": sum}, nil
}

func ociManifestDigest(team Team, origin, name, tag string) (map[string]string, error) {
	accept := "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
	sum, err := download(origin+"/v2/"+name+"/manifests/"+tag, team.Token, sha256.New(), map[string]string{"Accept": accept})
	if err != nil {
		return nil, err
	}
	return map[string]string{"sha256": sum}, nil
}

// download streams a file from the registry and returns its digest, hex.
func download(fileURL, token string, h hash.Hash, headers map[string]string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, fileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not download %s (%d)", fileURL, resp.StatusCode)
	}
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyTeam(only string) (Team, error) {
	config, err := loadConfig()
	if err != nil {
		return Team{}, err
	}
	if only != "" {
		team, ok := config.Teams[only]
		if !ok {
			return Team{}, fmt.Errorf("not signed in to %s: run polypack login", only)
		}
		return team, nil
	}
	switch slugs := config.slugs(); len(slugs) {
	case 0:
		return Team{}, errors.New("not signed in: run polypack login first")
	case 1:
		return config.Teams[slugs[0]], nil
	default:
		return Team{}, fmt.Errorf("signed in to several Teams (%s): say which with --team", strings.Join(slugs, ", "))
	}
}

func hostname(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}
