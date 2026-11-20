package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultHost = "https://polypack.io"

func cmdLogin(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	host := flags.String("host", envOr("POLYPACK_HOST", defaultHost), "PolyPack's address")
	hostname, _ := os.Hostname()
	name := flags.String("name", hostname, "what to call this machine")
	noSetup := flags.Bool("no-setup", false, "do not configure npm, Composer and docker")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		*name = "this machine"
	}

	ctx := context.Background()
	started, err := startDevice(ctx, *host, *name)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Your code: %s\n", started.UserCode)
	fmt.Fprintf(stdout, "Approve it at %s\n", started.VerificationURIComplete)
	fmt.Fprintln(stdout, "Waiting for approval in the browser…")
	openBrowser(started.VerificationURIComplete)

	result, err := pollDevice(ctx, *host, started)
	if err != nil {
		return err
	}

	config, err := loadConfig()
	if err != nil {
		return err
	}
	team := Team{
		Name:      result.Team.Name,
		Email:     result.User.Email,
		Host:      strings.TrimRight(*host, "/"),
		Token:     result.Token,
		ExpiresAt: result.ExpiresAt,
		NPM:       result.Registry.NPM,
		NPMScopes: result.Registry.NPMScopes,
		Composer:  result.Registry.Composer,
	}
	if result.Registry.OCIHost != nil {
		team.OCIHost = *result.Registry.OCIHost
	}
	config.Teams[result.Team.Slug] = team
	if err := config.save(); err != nil {
		return err
	}

	expires := ""
	if t, err := time.Parse(time.RFC3339, result.ExpiresAt); err == nil {
		expires = " until " + t.Format("2 Jan 2006")
	}
	fmt.Fprintf(stdout, "Signed in to %s as %s, with a read-only Token%s.\n", team.Name, team.Email, expires)

	if *noSetup {
		return nil
	}
	return setupTeam(result.Team.Slug, team, setupOptions{npm: true, composer: true, docker: true}, stdout, stderr)
}

type setupOptions struct{ npm, composer, docker bool }

func cmdSetup(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	noNPM := flags.Bool("no-npm", false, "leave npm alone")
	noComposer := flags.Bool("no-composer", false, "leave Composer alone")
	noDocker := flags.Bool("no-docker", false, "leave docker alone")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	if len(config.Teams) == 0 {
		return errors.New("not signed in: run polypack login first")
	}
	options := setupOptions{npm: !*noNPM, composer: !*noComposer, docker: !*noDocker}
	for _, slug := range config.slugs() {
		if err := setupTeam(slug, config.Teams[slug], options, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

// setupTeam writes the user's own config for one Team, never a project's.
func setupTeam(slug string, team Team, options setupOptions, stdout, stderr io.Writer) error {
	if options.npm {
		path, err := npmrcPath()
		if err != nil {
			return err
		}
		if err := editFile(path, func(content string) string { return upsertBlock(content, slug, npmBlockLines(team)) }); err != nil {
			return fmt.Errorf("could not update %s: %w", path, err)
		}
		if len(team.NPMScopes) > 0 {
			fmt.Fprintf(stdout, "npm: %s now install from %s (%s).\n", strings.Join(team.NPMScopes, ", "), team.Name, path)
		} else {
			fmt.Fprintf(stdout, "npm: the Token for %s is in %s.\n", team.NPM, path)
		}
	}

	if options.composer {
		home, err := composerHome()
		if err != nil {
			return err
		}
		authFile := filepath.Join(home, "auth.json")
		if err := setComposerBearer(authFile, composerAuthHost(team.Composer), team.Token); err != nil {
			return fmt.Errorf("could not update %s: %w", authFile, err)
		}
		fmt.Fprintf(stdout, "Composer: the Token is in %s. In each project, run:\n  composer config repositories.%s composer %s\n", authFile, slug, team.Composer)
	}

	if options.docker && team.OCIHost != "" {
		ran, err := dockerLogin(team.OCIHost, team.Token)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "docker: %v\n", err)
		case ran:
			fmt.Fprintf(stdout, "docker: logged in to %s.\n", team.OCIHost)
		default:
			fmt.Fprintf(stdout, "docker: not installed; skipped %s.\n", team.OCIHost)
		}
	}
	return nil
}

func cmdWhoami(args []string, stdout io.Writer) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	if len(config.Teams) == 0 {
		return errors.New("not signed in: run polypack login first")
	}
	for _, slug := range config.slugs() {
		team := config.Teams[slug]
		username, err := registryWhoami(team)
		if err != nil {
			fmt.Fprintf(stdout, "%s: %v\n", slug, err)
			continue
		}
		fmt.Fprintf(stdout, "%s: %s (%s)\n", slug, username, team.Name)
	}
	return nil
}

func registryWhoami(team Team) (string, error) {
	resp, data, err := registryRequest(http.MethodGet, team.NPM+"-/whoami", team.Token)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the Token was refused (%d): %s; run polypack login again", resp.StatusCode, errorText(data))
	}
	var body struct {
		Username string `json:"username"`
	}
	if err := jsonUnmarshal(data, &body); err != nil {
		return "", err
	}
	return body.Username, nil
}

func cmdLogout(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("logout", flag.ContinueOnError)
	flags.SetOutput(stderr)
	only := flags.String("team", "", "sign out of this Team only")
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	slugs := config.slugs()
	if *only != "" {
		if _, ok := config.Teams[*only]; !ok {
			return fmt.Errorf("not signed in to %s", *only)
		}
		slugs = []string{*only}
	}
	if len(slugs) == 0 {
		fmt.Fprintln(stdout, "Not signed in.")
		return nil
	}

	for _, slug := range slugs {
		team := config.Teams[slug]
		// npm's own logout: the Token revokes itself.
		resp, data, err := registryRequest(http.MethodDelete, team.NPM+"-/user/token/"+url.PathEscape(team.Token), team.Token)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "%s: could not reach the registry (%v); revoke the Token under My Tokens.\n", slug, err)
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized:
			// Revoked now, or already.
		default:
			fmt.Fprintf(stderr, "%s: the registry answered %d (%s); revoke the Token under My Tokens.\n", slug, resp.StatusCode, errorText(data))
		}

		if path, err := npmrcPath(); err == nil {
			_ = editFile(path, func(content string) string { return removeBlock(content, slug) })
		}
		if home, err := composerHome(); err == nil {
			_ = setComposerBearer(filepath.Join(home, "auth.json"), composerAuthHost(team.Composer), "")
		}
		if team.OCIHost != "" {
			dockerLogout(team.OCIHost)
		}
		delete(config.Teams, slug)
		fmt.Fprintf(stdout, "Signed out of %s; its Token is revoked.\n", team.Name)
	}
	return config.save()
}

func registryRequest(method, url, token string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, data, err
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
