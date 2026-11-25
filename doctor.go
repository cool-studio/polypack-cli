package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// `polypack doctor` (step 71, ADR-0035): checks what npm, Composer and docker
// on this machine (and in this project) send to each Team's hosts, and asks
// each host what it makes of it. It changes nothing, and never prints a
// Token: only what the panel shows, `polypack_…` and its last four characters.

// doctorClient makes the doctor's requests; tests point it at a fake host.
var doctorClient = &http.Client{Timeout: 15 * time.Second}

// A Token expiring sooner than this is a warning.
const expiringSoon = 14 * 24 * time.Hour

type level int

const (
	levelOK level = iota
	levelInfo
	levelWarn
	levelFail
)

var marks = map[level]string{levelOK: "✓", levelInfo: "·", levelWarn: "!", levelFail: "✗"}

type report struct {
	out      io.Writer
	problems int
	warnings int
}

func (r *report) line(indent int, l level, text, hint string) {
	pad := strings.Repeat("  ", indent)
	fmt.Fprintf(r.out, "%s%s %s\n", pad, marks[l], text)
	if hint != "" {
		fmt.Fprintf(r.out, "%s  %s\n", pad, hint)
	}
	switch l {
	case levelFail:
		r.problems++
	case levelWarn:
		r.warnings++
	}
}

func (r *report) heading(indent int, text string) {
	fmt.Fprintf(r.out, "%s%s\n", strings.Repeat("  ", indent), text)
}

// target is one Team's hosts, from the CLI's config or found in npm's or
// Composer's config.
type target struct {
	slug      string
	name      string
	npm       string // the npm Registry's URL, ending in /npm/
	scopes    []string
	composer  string // the Composer Registry's URL
	ociHost   string
	cli       *Team // the CLI's own sign-in, if any
	foundOnly bool  // not signed in with the CLI; found in a package manager's config
}

func (t target) origin() string {
	u, err := url.Parse(t.npm)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (t target) host() string {
	if u, err := url.Parse(t.npm); err == nil {
		return u.Host
	}
	return t.npm
}

// The OCI host is served the same way as the npm one: same scheme.
func (t target) ociOrigin() string {
	if u, err := url.Parse(t.npm); err == nil && u.Scheme != "" {
		return u.Scheme + "://" + t.ociHost
	}
	return "https://" + t.ociHost
}

func cmdDoctor(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	only := flags.String("team", "", "check only this Team")
	if err := flags.Parse(args); err != nil {
		return err
	}

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("could not read the CLI's config: %w", err)
	}
	path, _ := configPath()
	r := &report{out: stdout}
	fmt.Fprintf(stdout, "polypack %s on %s/%s\n", version, runtime.GOOS, runtime.GOARCH)

	d := newDoctor()
	targets := d.targets(config)
	if *only != "" {
		var kept []target
		for _, t := range targets {
			if t.slug == *only {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			return fmt.Errorf("no Team %q is signed in, or set up in npm or Composer", *only)
		}
		targets = kept
	}

	if len(config.Teams) == 0 {
		r.line(0, levelInfo, "Not signed in with polypack login ("+tidy(path)+").", "")
	} else {
		r.line(0, levelOK, fmt.Sprintf("Signed in to %s (%s).", strings.Join(config.slugs(), ", "), tidy(path)), "")
	}
	if len(targets) == 0 {
		r.line(0, levelFail, "No PolyPack registry is set up in npm or Composer here.", "Run polypack login, which sets them up.")
	}

	for _, t := range targets {
		fmt.Fprintln(stdout)
		d.checkTarget(r, t)
	}

	fmt.Fprintln(stdout)
	switch {
	case r.problems > 0:
		return fmt.Errorf("%s found", plural(r.problems, "problem"))
	case r.warnings > 0:
		fmt.Fprintf(stdout, "No problems, %s.\n", plural(r.warnings, "warning"))
	default:
		fmt.Fprintln(stdout, "Everything looks right.")
	}
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

type doctor struct {
	cwd       string
	npmFiles  []npmrcFile // highest precedence first: the project's, then the user's
	described map[string]tokenInfo
}

func newDoctor() *doctor {
	cwd, _ := os.Getwd()
	d := &doctor{cwd: cwd, described: map[string]tokenInfo{}}
	if project := npmProjectRoot(cwd); project != "" {
		if f, ok := readNpmrc(filepath.Join(project, ".npmrc"), "this project's .npmrc"); ok {
			d.npmFiles = append(d.npmFiles, f)
		}
	}
	if path, err := npmrcPath(); err == nil {
		if f, ok := readNpmrc(path, path); ok {
			d.npmFiles = append(d.npmFiles, f)
		}
	}
	return d
}

// registryDomains are the domains whose hosts are PolyPack Teams':
// POLYPACK_REGISTRY_DOMAIN, the signed-in Teams', and production's.
func registryDomains(config *Config) []string {
	seen := map[string]bool{}
	var domains []string
	add := func(domain string) {
		if domain != "" && !seen[domain] {
			seen[domain] = true
			domains = append(domains, domain)
		}
	}
	add(os.Getenv("POLYPACK_REGISTRY_DOMAIN"))
	for _, slug := range config.slugs() {
		if u, err := url.Parse(config.Teams[slug].NPM); err == nil {
			if _, rest, ok := strings.Cut(u.Hostname(), "."); ok {
				add(rest)
			}
		}
	}
	add("pkg.polypack.io")
	return domains
}

// teamSlugOf gives the Team a registry host belongs to, if it is PolyPack's.
func teamSlugOf(host string, domains []string) string {
	hostname := host
	if h, _, err := splitHostPort(host); err == nil {
		hostname = h
	}
	for _, domain := range domains {
		if slug, ok := strings.CutSuffix(hostname, "."+domain); ok && slug != "" && !strings.Contains(slug, ".") {
			return slug
		}
	}
	return ""
}

func splitHostPort(host string) (string, string, error) {
	u, err := url.Parse("//" + host)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}

// targets are the signed-in Teams, then any other PolyPack hosts npm's
// scopes or Composer's auth.json point at.
func (d *doctor) targets(config *Config) []target {
	var targets []target
	known := map[string]bool{}
	for _, slug := range config.slugs() {
		team := config.Teams[slug]
		t := target{slug: slug, name: team.Name, npm: team.NPM, scopes: team.NPMScopes, composer: team.Composer, ociHost: team.OCIHost, cli: &team}
		targets = append(targets, t)
		known[t.host()] = true
	}

	domains := registryDomains(config)
	found := map[string]*target{}
	var order []string
	for _, file := range d.npmFiles {
		for _, key := range file.keys {
			scope, ok := strings.CutSuffix(key, ":registry")
			if !ok || !strings.HasPrefix(scope, "@") {
				continue
			}
			u, err := url.Parse(file.values[key])
			if err != nil || u.Host == "" || known[u.Host] {
				continue
			}
			slug := teamSlugOf(u.Host, domains)
			if slug == "" {
				continue
			}
			t, ok := found[u.Host]
			if !ok {
				t = &target{slug: slug, npm: u.Scheme + "://" + u.Host + "/npm/", composer: u.Scheme + "://" + u.Host + "/composer", foundOnly: true}
				found[u.Host] = t
				order = append(order, u.Host)
			}
			if !contains(t.scopes, scope) {
				t.scopes = append(t.scopes, scope)
			}
		}
	}
	for _, source := range d.composerSources() {
		for _, host := range source.hosts() {
			if known[host] || found[host] != nil {
				continue
			}
			if slug := teamSlugOf(host, domains); slug != "" {
				found[host] = &target{slug: slug, npm: "https://" + host + "/npm/", composer: "https://" + host + "/composer", foundOnly: true}
				order = append(order, host)
			}
		}
	}
	for _, host := range order {
		targets = append(targets, *found[host])
	}
	return targets
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func (d *doctor) checkTarget(r *report, t target) {
	title := t.slug
	if t.name != "" {
		title += " (" + t.name + ")"
	}
	if t.foundOnly {
		title += ", found in your config; not signed in with the CLI"
	}
	r.heading(0, title)

	// The host first: if it cannot be reached, nothing else can be checked.
	if !d.checkHost(r, 1, t.origin(), t.host()) {
		return
	}

	if t.cli != nil {
		info := d.describe(t.origin(), t.cli.Token)
		l, text, hint := judge(info, t.cli.Token, true)
		r.line(1, l, "The CLI's Token: "+text, hint)
	}

	d.checkNpm(r, t)
	d.checkComposer(r, t)
	if t.ociHost != "" {
		d.checkDocker(r, t)
	}
}

// checkHost asks the host anonymously: a 401 saying no Token was sent means
// it is a PolyPack Team's registry, up and serving.
func (d *doctor) checkHost(r *report, indent int, origin, host string) bool {
	info := d.describe(origin, "")
	switch {
	case info.err != nil:
		r.line(indent, levelFail, "Cannot reach "+host+": "+info.err.Error(), "Check your network, proxy or VPN.")
		return false
	case info.httpStatus == http.StatusNotFound:
		r.line(indent, levelFail, host+" is not a PolyPack Team's registry.", "Check the Team's slug; it may have been renamed.")
		return false
	case info.httpStatus == http.StatusForbidden:
		r.line(indent, levelFail, host+": "+info.Message, "")
		return false
	case info.Status == "none":
		r.line(indent, levelOK, host+" answers.", "")
		return true
	default:
		r.line(indent, levelFail, fmt.Sprintf("%s answered unexpectedly (HTTP %d).", host, info.httpStatus), "")
		return false
	}
}

// --- npm ---------------------------------------------------------------------

func (d *doctor) checkNpm(r *report, t target) {
	r.heading(1, "npm")
	if len(t.scopes) == 0 {
		r.line(2, levelInfo, "This Team has no npm scopes, so npm installs nothing from it.", "")
		return
	}

	want := strings.TrimRight(t.npm, "/") + "/"
	var registries []string
	for _, scope := range t.scopes {
		value, source := d.npmValue(scope + ":registry")
		switch {
		case value == "":
			r.line(2, levelFail, scope+" has no registry set, so npm looks for it on registry.npmjs.org.", "Run polypack setup --npm.")
		case strings.TrimRight(value, "/")+"/" != want:
			r.line(2, levelFail, fmt.Sprintf("%s installs from %s (%s), not from %s.", scope, value, tidy(source), want), "Remove that line, or run polypack setup --npm.")
		default:
			r.line(2, levelOK, fmt.Sprintf("%s installs from %s (%s).", scope, value, tidy(source)), "")
			if !contains(registries, want) {
				registries = append(registries, want)
			}
		}
	}

	for _, registry := range registries {
		key := npmAuthKey(registry) + ":_authToken"
		value, source := d.npmValue(key)
		if value == "" {
			r.line(2, levelFail, "npm sends no Token to "+npmAuthKey(registry)+", so only public Packs install.", "Run polypack setup --npm.")
			continue
		}
		if missing := unsetVariables(value); len(missing) > 0 {
			r.line(2, levelFail, fmt.Sprintf("npm's Token (%s) is %s, which is not set.", tidy(source), strings.Join(missing, ", ")), "Set it, or run polypack setup --npm.")
			continue
		}
		d.sends(r, t, t.origin(), "npm", expandVariables(value), source)
	}
}

// sends reports the Token a package manager sends. The CLI's own Token was
// judged once already, so it is named, not judged again: one expired Token
// is one problem.
func (d *doctor) sends(r *report, t target, origin, tool, secret, source string) {
	if t.cli != nil && secret == t.cli.Token && origin == t.origin() {
		r.line(2, levelOK, fmt.Sprintf("%s sends the CLI's Token (%s, %s).", tool, display(secret), tidy(source)), "")
		return
	}
	l, text, hint := judge(d.describe(origin, secret), secret, false)
	r.line(2, l, fmt.Sprintf("%s sends %s (%s): %s", tool, display(secret), tidy(source), text), hint)
}

// tidy shortens a path in the home directory to ~/….
func tidy(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// npmrcFile is one .npmrc, its keys in order.
type npmrcFile struct {
	source string
	keys   []string
	values map[string]string
}

func readNpmrc(path, source string) (npmrcFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return npmrcFile{}, false
	}
	f := npmrcFile{source: source, values: map[string]string{}}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(value), `"'`)
		if _, seen := f.values[key]; !seen {
			f.keys = append(f.keys, key)
		}
		f.values[key] = value // a later line wins, as in npm
	}
	return f, true
}

// npmValue is what npm uses for a key: the project's .npmrc first, then the
// user's.
func (d *doctor) npmValue(key string) (string, string) {
	for _, f := range d.npmFiles {
		if value, ok := f.values[key]; ok {
			return value, f.source
		}
	}
	return "", ""
}

// npmProjectRoot is where npm looks for a project's .npmrc: the nearest
// directory up from here with a package.json or node_modules.
func npmProjectRoot(dir string) string {
	for dir != "" {
		for _, marker := range []string{"package.json", "node_modules"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

var variablePattern = regexp.MustCompile(`\$\{([^}?]+)\??\}`)

func unsetVariables(value string) []string {
	var missing []string
	for _, match := range variablePattern.FindAllStringSubmatch(value, -1) {
		if os.Getenv(match[1]) == "" {
			missing = append(missing, "${"+match[1]+"}")
		}
	}
	return missing
}

func expandVariables(value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(match string) string {
		return os.Getenv(variablePattern.FindStringSubmatch(match)[1])
	})
}

// --- Composer ----------------------------------------------------------------

type composerSource struct {
	source string
	auth   map[string]any
}

// hosts are the hosts this auth config has credentials for.
func (s composerSource) hosts() []string {
	var hosts []string
	for _, kind := range []string{"bearer", "http-basic"} {
		if entries, ok := s.auth[kind].(map[string]any); ok {
			for host := range entries {
				hosts = append(hosts, host)
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

// secret is the Token this config sends to a host, and how.
func (s composerSource) secret(host string) (string, string) {
	if entries, ok := s.auth["bearer"].(map[string]any); ok {
		if token, ok := entries[host].(string); ok && token != "" {
			return token, "bearer"
		}
	}
	if entries, ok := s.auth["http-basic"].(map[string]any); ok {
		if entry, ok := entries[host].(map[string]any); ok {
			password, _ := entry["password"].(string)
			username, _ := entry["username"].(string)
			if password != "" {
				return password, "http-basic"
			}
			if username != "" {
				return username, "http-basic"
			}
		}
	}
	return "", ""
}

// composerSources, in the order Composer prefers them: the project's
// auth.json, COMPOSER_AUTH, then the global auth.json.
func (d *doctor) composerSources() []composerSource {
	var sources []composerSource
	read := func(path, source string) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		auth := map[string]any{}
		if json.Unmarshal(data, &auth) == nil {
			sources = append(sources, composerSource{source: source, auth: auth})
		}
	}
	read(filepath.Join(d.cwd, "auth.json"), "this project's auth.json")
	if env := os.Getenv("COMPOSER_AUTH"); env != "" {
		auth := map[string]any{}
		if json.Unmarshal([]byte(env), &auth) == nil {
			sources = append(sources, composerSource{source: "COMPOSER_AUTH", auth: auth})
		}
	}
	if home, err := composerHome(); err == nil {
		path := filepath.Join(home, "auth.json")
		read(path, path)
	}
	return sources
}

func (d *doctor) checkComposer(r *report, t target) {
	r.heading(1, "Composer")
	host := composerAuthHost(t.composer)
	var secret, how, source string
	for _, s := range d.composerSources() {
		if secret, how = s.secret(host); secret != "" {
			source = s.source
			break
		}
	}
	if secret == "" {
		r.line(2, levelFail, "Composer sends no Token to "+host+", so only public Packs install.", "Run polypack setup --composer.")
	} else {
		d.sends(r, t, t.origin(), "Composer", secret, how+", "+tidy(source))
	}

	// The project's composer.json, if there is one here.
	data, err := os.ReadFile(filepath.Join(d.cwd, "composer.json"))
	if err != nil {
		return
	}
	var project struct {
		Repositories json.RawMessage `json:"repositories"`
	}
	if json.Unmarshal(data, &project) != nil {
		r.line(2, levelWarn, "This project's composer.json is not valid JSON.", "")
		return
	}
	if composerUses(project.Repositories, t.composer) {
		r.line(2, levelOK, "This project's composer.json uses "+t.composer+".", "")
	} else {
		r.line(2, levelInfo, "This project's composer.json has no repository for "+t.composer+".",
			fmt.Sprintf("If it needs %s's Packs, run: composer config repositories.%s composer %s", t.slug, t.slug, t.composer))
	}
}

// composerUses says whether composer.json's repositories (a list, or an
// object keyed by name) include a Composer repository at this URL.
func composerUses(raw json.RawMessage, want string) bool {
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		var named map[string]any
		if json.Unmarshal(raw, &named) != nil {
			return false
		}
		for _, repo := range named {
			if m, ok := repo.(map[string]any); ok {
				list = append(list, m)
			}
		}
	}
	for _, repo := range list {
		if u, _ := repo["url"].(string); strings.TrimRight(u, "/") == strings.TrimRight(want, "/") {
			return true
		}
	}
	return false
}

// --- docker ------------------------------------------------------------------

func (d *doctor) checkDocker(r *report, t target) {
	r.heading(1, "docker")
	if !d.checkHost(r, 2, t.ociOrigin(), t.ociHost) {
		return
	}
	secret, source, err := dockerCredential(t.ociHost)
	switch {
	case err != nil:
		r.line(2, levelWarn, "Could not read docker's credentials for "+t.ociHost+": "+err.Error(), "")
	case secret == "":
		if _, err := exec.LookPath("docker"); err != nil && source == "" {
			r.line(2, levelInfo, "docker is not installed.", "")
			return
		}
		r.line(2, levelWarn, "docker is not logged in to "+t.ociHost+", so it pulls only public images.", "Run polypack setup --docker.")
	default:
		d.sends(r, t, t.ociOrigin(), "docker", secret, source)
	}
}

// dockerCredential is the password docker sends to a host: from its
// config.json, or from the credential helper that config names.
func dockerCredential(host string) (string, string, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		dir = filepath.Join(home, ".docker")
	}
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var config struct {
		Auths       map[string]struct{ Auth string } `json:"auths"`
		CredsStore  string                           `json:"credsStore"`
		CredHelpers map[string]string                `json:"credHelpers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", fmt.Errorf("%s is not valid JSON", path)
	}

	helper := config.CredHelpers[host]
	if helper == "" {
		if entry, ok := config.Auths[host]; ok && entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return "", "", fmt.Errorf("its entry in %s is not valid", path)
			}
			_, password, _ := strings.Cut(string(decoded), ":")
			return password, path, nil
		}
		helper = config.CredsStore
	}
	if helper == "" {
		return "", path, nil
	}

	program, err := exec.LookPath("docker-credential-" + helper)
	if err != nil {
		return "", path, fmt.Errorf("docker's credential helper docker-credential-%s is not installed", helper)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, "get")
	cmd.Stdin = strings.NewReader(host)
	out, err := cmd.Output()
	if err != nil {
		// Helpers fail with "credentials not found in native keychain".
		return "", "docker-credential-" + helper, nil
	}
	var credential struct{ Secret string }
	if err := json.Unmarshal(out, &credential); err != nil {
		return "", "", fmt.Errorf("docker-credential-%s answered unexpectedly", helper)
	}
	return credential.Secret, "docker-credential-" + helper, nil
}

// --- asking the host ---------------------------------------------------------

// tokenInfo is what GET /-/polypack/v1/token answered.
type tokenInfo struct {
	Status    string   `json:"status"`
	Message   string   `json:"message"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Username  string   `json:"username"`
	Abilities []string `json:"abilities"`
	ExpiresAt *string  `json:"expires_at"`
	Packs     []string `json:"packs"`
	Team      string   `json:"team"`

	httpStatus int
	err        error
}

func (d *doctor) describe(origin, secret string) tokenInfo {
	key := origin + "\x00" + secret
	if info, ok := d.described[key]; ok {
		return info
	}
	info := describeToken(origin, secret)
	d.described[key] = info
	return info
}

func describeToken(origin, secret string) tokenInfo {
	req, err := http.NewRequest(http.MethodGet, origin+"/-/polypack/v1/token", nil)
	if err != nil {
		return tokenInfo{err: err}
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := doctorClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return tokenInfo{err: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	info := tokenInfo{httpStatus: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized:
		if json.Unmarshal(data, &info) != nil || info.Status == "" {
			// An older PolyPack, without the endpoint, or something else on that host.
			info.Status, info.Message = "unanswered", "the host did not say what it makes of this Token."
		}
	case http.StatusForbidden:
		// An address the Token's list does not allow, in the registry's own
		// shape (npm and Composer: error; OCI: errors), or a suspended Team.
		info.Status, info.Message = "refused", refusedMessage(data)
	case http.StatusTooManyRequests:
		info.Status, info.Message = "unanswered", "PolyPack asked to slow down. Run polypack doctor again in a minute."
	default:
		info.Status, info.Message = "unanswered", fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	info.httpStatus = resp.StatusCode
	return info
}

// refusedMessage is a 403's reason: OCI's {"errors": [{"message"}]}, or
// either of PolyPack's other shapes, or a plain-text body.
func refusedMessage(data []byte) string {
	var oci struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &oci) == nil && len(oci.Errors) > 0 && oci.Errors[0].Message != "" {
		return oci.Errors[0].Message
	}
	return errorText(data)
}

// judge turns the host's answer into a line. cli: the Token is the CLI's
// own, which `polypack login` replaces.
func judge(info tokenInfo, secret string, cli bool) (level, string, string) {
	renew := "Issue a new Token in PolyPack (My Tokens, or ask an Admin) and put it here."
	if cli {
		renew = "Run polypack login to get a new one."
	}
	if info.err != nil {
		return levelFail, "could not be checked: " + info.err.Error(), ""
	}
	switch info.Status {
	case "active":
		text := describeActive(info)
		if info.ExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *info.ExpiresAt); err == nil && time.Until(t) < expiringSoon {
				return levelWarn, text, "It expires soon. " + renew
			}
		}
		return levelOK, text, ""
	case "expired", "revoked":
		return levelFail, info.Message, renew
	case "other_team":
		return levelFail, info.Message, "Use a Token of this Team: " + renew
	case "malformed", "unknown":
		return levelFail, info.Message, "Check it was copied whole, or: " + renew
	case "refused":
		return levelFail, info.Message, "An Admin can change the addresses it may be used from, in PolyPack."
	default:
		return levelWarn, "could not be checked: " + info.Message, ""
	}
}

var kindNames = map[string]string{"member": "Member Token", "team": "Team Token", "client": "Client Token", "publisher": "Publisher Token"}

func describeActive(info tokenInfo) string {
	kind := kindNames[info.Kind]
	if kind == "" {
		kind = "Token"
	}
	ability := "read-only"
	if contains(info.Abilities, "publish") {
		ability = "read and publish"
	}
	text := fmt.Sprintf("%q, a %s %s", info.Name, ability, kind)
	if info.Kind == "member" && info.Username != "" {
		text += " of " + info.Username
	}
	if info.ExpiresAt != nil {
		if t, err := time.Parse(time.RFC3339, *info.ExpiresAt); err == nil {
			text += ", until " + t.Format("2 Jan 2006")
		}
	} else {
		text += ", with no expiry"
	}
	switch {
	case info.Packs == nil:
	case len(info.Packs) == 0:
		text += ", limited to Packs that no longer exist"
	default:
		text += ", limited to " + strings.Join(info.Packs, ", ")
	}
	return text + "."
}

// display is what the panel shows instead of a Token: never the Token.
func display(secret string) string {
	if !strings.HasPrefix(secret, "polypack_") || len(secret) < 13 {
		return "a value that is not a PolyPack Token"
	}
	return "polypack_…" + secret[len(secret)-4:]
}
