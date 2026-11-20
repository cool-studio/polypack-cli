package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// The device sign-in (ADR-0026), modelled on RFC 8628.

type deviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceResult struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	User      struct {
		Email string `json:"email"`
	} `json:"user"`
	Team struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"team"`
	Registry struct {
		NPM       string   `json:"npm"`
		NPMScopes []string `json:"npm_scopes"`
		Composer  string   `json:"composer"`
		OCIHost   *string  `json:"oci_host"`
	} `json:"registry"`
}

// slowDownStep is how much longer to wait after `slow_down`; tests shorten it.
var slowDownStep = 5 * time.Second

var httpClient = &http.Client{Timeout: 30 * time.Second}

func userAgent() string {
	return fmt.Sprintf("polypack-cli/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

func postJSON(ctx context.Context, url string, body any) (*http.Response, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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

func startDevice(ctx context.Context, host, name string) (*deviceStart, error) {
	resp, data, err := postJSON(ctx, strings.TrimRight(host, "/")+"/cli/v1/device", map[string]string{"client_name": name})
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", host, err)
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("%s refused to start a sign-in (%d): %s", host, resp.StatusCode, errorText(data))
	}
	var started deviceStart
	if err := json.Unmarshal(data, &started); err != nil {
		return nil, fmt.Errorf("unexpected answer from %s: %w", host, err)
	}
	return &started, nil
}

// pollDevice waits for the person to approve, as the server asks: every
// interval seconds, slower after slow_down, until the code expires.
func pollDevice(ctx context.Context, host string, started *deviceStart) (*deviceResult, error) {
	interval := time.Duration(started.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(started.ExpiresIn) * time.Second)

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the code expired before it was approved: run polypack login again")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		resp, data, err := postJSON(ctx, strings.TrimRight(host, "/")+"/cli/v1/device/token", map[string]string{"device_code": started.DeviceCode})
		if err != nil {
			// A dropped connection is worth another try, like a pending answer.
			continue
		}
		if resp.StatusCode == http.StatusOK {
			var result deviceResult
			if err := json.Unmarshal(data, &result); err != nil {
				return nil, fmt.Errorf("unexpected answer from %s: %w", host, err)
			}
			return &result, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			interval += slowDownStep
			continue
		}

		switch code := errorCode(data); code {
		case "authorization_pending":
		case "slow_down":
			interval += slowDownStep
		case "access_denied":
			return nil, fmt.Errorf("the sign-in was denied in the browser")
		case "expired_token":
			return nil, fmt.Errorf("the code expired or was already used: run polypack login again")
		default:
			return nil, fmt.Errorf("%s answered %d: %s", host, resp.StatusCode, errorText(data))
		}
	}
}

func errorCode(data []byte) string {
	var body struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil {
		if code, ok := body.Error.(string); ok {
			return code
		}
	}
	return ""
}

// errorText is the server's message, from either of PolyPack's error shapes.
func errorText(data []byte) string {
	var body struct {
		Error       any    `json:"error"`
		Description string `json:"error_description"`
		Message     string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		switch {
		case body.Description != "":
			return body.Description
		case body.Message != "":
			return body.Message
		}
		if text, ok := body.Error.(string); ok && text != "" {
			return text
		}
	}
	text := strings.TrimSpace(string(data))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

func jsonUnmarshal(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unexpected answer: %w", err)
	}
	return nil
}
