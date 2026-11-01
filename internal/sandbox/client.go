// Package sandbox implements the E2B control API and envd process transport.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client holds one provider connection; credentials stay in the control plane.
type Client struct {
	Provider   string
	APIURL     string
	Domain     string
	APIKey     string
	HTTPClient *http.Client
}

// Sandbox is a provider workspace and its in-memory access credentials.
type Sandbox struct {
	ID          string            `json:"sandboxID"`
	Domain      string            `json:"domain"`
	EnvdVersion string            `json:"envdVersion"`
	Metadata    map[string]string `json:"metadata"`
	State       string            `json:"state"`
	EndAt       time.Time         `json:"endAt"`
	client      *Client
	accessToken string
	envdURL     string
}

// String describes a sandbox without exposing its scoped credentials.
func (s *Sandbox) String() string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("sandbox %s (%s, envd %s)", s.ID, s.State, s.EnvdVersion)
}

// CreateOptions configures a sandbox independently of the worker command.
type CreateOptions struct {
	TemplateID     string
	Timeout        time.Duration
	Env            map[string]string
	Metadata       map[string]string
	PauseOnTimeout bool
}

// HTTPError exposes the status without including provider response credentials.
type HTTPError struct {
	StatusCode int
	Operation  string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("sandbox %s: HTTP %d", e.Operation, e.StatusCode)
}

// IsNotFound reports an already removed sandbox or missing file.
func IsNotFound(err error) bool {
	var e *HTTPError
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

func (c *Client) domain() string {
	if c.Domain != "" {
		return c.Domain
	}
	if c.Provider == "novita" {
		return "us-phx-1.sandbox.novita.ai"
	}
	return "e2b.app"
}

func (c *Client) httpClient() *http.Client {
	var client http.Client
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	// An API redirect must never forward its credentials to another origin.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (c *Client) apiURL() string {
	if c.APIURL != "" {
		return strings.TrimRight(c.APIURL, "/")
	}
	return "https://api." + c.domain()
}

func (c *Client) request(ctx context.Context, method, path string, body any, target any) (http.Header, error) {
	var data io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		data = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL()+path, data)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{resp.StatusCode, method + " " + req.URL.Path}
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target); err != nil {
			return nil, fmt.Errorf("decode sandbox response: %w", err)
		}
	}
	return resp.Header, nil
}

func (c *Client) readSandbox(ctx context.Context, method, path string, body any) (*Sandbox, error) {
	var result struct {
		Sandbox
		AccessToken string `json:"envdAccessToken"`
	}
	if _, err := c.request(ctx, method, path, body, &result); err != nil {
		return nil, err
	}
	if result.ID == "" || strings.ContainsAny(result.ID, "/\\?# \t\r\n") {
		return nil, errors.New("provider returned an invalid sandbox ID")
	}
	result.client, result.accessToken = c, result.AccessToken
	if result.Domain == "" {
		result.Domain = c.domain()
	}
	if strings.ContainsAny(result.Domain, "/\\?#:@ \t\r\n") {
		return nil, errors.New("provider returned an invalid sandbox domain")
	}
	result.envdURL = "https://49983-" + result.ID + "." + result.Domain
	return &result.Sandbox, nil
}

func seconds(timeout time.Duration) int64 { return int64(math.Ceil(timeout.Seconds())) }

// Create provisions a secure sandbox with public application traffic disabled.
func (c *Client) Create(ctx context.Context, options CreateOptions) (*Sandbox, error) {
	if options.TemplateID == "" {
		return nil, errors.New("sandbox template ID is required")
	}
	if options.Timeout <= 0 {
		return nil, errors.New("sandbox timeout must be positive")
	}
	body := map[string]any{"templateID": options.TemplateID, "timeout": seconds(options.Timeout), "allow_internet_access": true, "network": map[string]bool{"allowPublicTraffic": false}, "autoPause": options.PauseOnTimeout}
	path := "/v2/sandboxes"
	if c.Provider == "novita" {
		path = "/sandboxes"
		body["secure"] = true
	} else if options.PauseOnTimeout {
		body["autoPauseMemory"] = true
	}
	if len(options.Metadata) > 0 {
		body["metadata"] = options.Metadata
	}
	if len(options.Env) > 0 {
		body["envVars"] = options.Env
	}
	if options.PauseOnTimeout {
		body["autoResume"] = map[string]bool{"enabled": false}
	}
	return c.readSandbox(ctx, http.MethodPost, path, body)
}

// Get reads the current sandbox state and refreshes its access credentials.
func (c *Client) Get(ctx context.Context, id string) (*Sandbox, error) {
	return c.readSandbox(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil)
}

// Connect resumes a paused sandbox, or attaches to a running one, with a new lease.
func (c *Client) Connect(ctx context.Context, id string, timeout time.Duration) (*Sandbox, error) {
	if timeout <= 0 {
		return nil, errors.New("sandbox timeout must be positive")
	}
	path := "/v2/sandboxes/"
	if c.Provider == "novita" {
		path = "/sandboxes/"
	}
	return c.readSandbox(ctx, http.MethodPost, path+url.PathEscape(id)+"/connect", map[string]any{"timeout": seconds(timeout)})
}

// SetTimeout renews the sandbox lease independently of command timeouts.
func (c *Client) SetTimeout(ctx context.Context, id string, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("sandbox timeout must be positive")
	}
	_, err := c.request(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/timeout", map[string]any{"timeout": seconds(timeout)}, nil)
	return err
}

// Pause retains sandbox disk and memory; an already paused response is successful.
func (c *Client) Pause(ctx context.Context, id string) error {
	var body any
	if c.Provider != "novita" {
		body = map[string]bool{"memory": true}
	}
	_, err := c.request(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause", body, nil)
	var e *HTTPError
	if errors.As(err, &e) && e.StatusCode == http.StatusConflict {
		current, readErr := c.Get(ctx, id)
		if readErr == nil && current.State == "paused" {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
	return err
}

// Kill removes a sandbox; repeated cleanup of a missing sandbox succeeds.
func (c *Client) Kill(ctx context.Context, id string) error {
	_, err := c.request(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// List returns all running and paused sandboxes matching the metadata filter.
func (c *Client) List(ctx context.Context, metadata map[string]string) ([]Sandbox, error) {
	filter := url.Values{}
	for k, v := range metadata {
		filter.Set(k, v)
	}
	query := url.Values{"state": {"running,paused"}, "limit": {"100"}}
	if len(filter) > 0 {
		query.Set("metadata", filter.Encode())
	}
	var result []Sandbox
	seen := map[string]bool{}
	for {
		var page []Sandbox
		headers, err := c.request(ctx, http.MethodGet, "/v2/sandboxes?"+query.Encode(), nil, &page)
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		next := headers.Get("X-Next-Token")
		if next == "" {
			return result, nil
		}
		if seen[next] {
			return nil, errors.New("sandbox listing returned a repeated pagination token")
		}
		seen[next] = true
		query.Set("nextToken", next)
	}
}
