package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/e2b-dev/infra/packages/runtime-bridge/backend"
)

type Config struct {
	URL    string
	Token  string
	Tenant string
	Branch string
	Region string
}

type Client struct {
	config Config
	http   *http.Client
	poll   time.Duration
}

func New(config Config) (*Client, error) {
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid compute URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return nil, errors.New("compute URL requires HTTPS except on loopback")
	}
	if config.Token == "" || !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(config.Tenant) || config.Branch == "" {
		return nil, errors.New("compute token, tenant and isolated branch are required")
	}

	return &Client{config: config, poll: time.Second, http: &http.Client{
		Timeout:       65 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	u := strings.TrimRight(c.config.URL, "/") + "/v1/" + url.PathEscape(c.config.Tenant) + "/services" + path + "?branch=" + url.QueryEscape(c.config.Branch)
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("compute request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return backend.ErrNotFound
	}
	if resp.StatusCode == http.StatusConflict {
		return backend.ErrConflict
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("compute %s returned HTTP %d", method, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
	}

	return nil
}

func (c *Client) Create(ctx context.Context, spec backend.Spec) error {
	body := map[string]any{
		"name": spec.Name, "image": spec.Image, "branch": c.config.Branch,
		"port": 0, "args": []string{"sh", "-c", "exec sleep infinity"},
		"env": spec.Env, "cpu_milli": spec.CPU * 1000, "memory_mib": spec.MemoryMiB,
		"always_on": true, "replicas": 1,
	}
	if c.config.Region != "" {
		body["region"] = c.config.Region
	}
	if err := c.request(ctx, http.MethodPost, "", body, nil); err != nil {
		return err
	}

	return c.wait(ctx, spec.Name, false)
}

func (c *Client) Get(ctx context.Context, name string) (backend.Instance, error) {
	var instance backend.Instance
	err := c.request(ctx, http.MethodGet, "/"+url.PathEscape(name), nil, &instance)

	return instance, err
}

func (c *Client) Delete(ctx context.Context, name string) error {
	err := c.request(ctx, http.MethodDelete, "/"+url.PathEscape(name), nil, nil)
	if errors.Is(err, backend.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	return c.wait(ctx, name, true)
}

func (c *Client) wait(ctx context.Context, name string, deleting bool) error {
	for {
		instance, err := c.Get(ctx, name)
		if deleting && errors.Is(err, backend.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !deleting && instance.State == "running" {
			return nil
		}
		if instance.State == "failed" {
			return errors.New("compute operation failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll):
		}
	}
}

func (c *Client) Exec(ctx context.Context, name string, cmd []string) (backend.Result, error) {
	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode *int32 `json:"exit_code"`
	}
	err := c.request(ctx, http.MethodPost, "/"+url.PathEscape(name)+"/exec", map[string]any{"cmd": cmd, "timeout_s": 60}, &result)
	if err != nil {
		return backend.Result{}, err
	}
	if result.ExitCode == nil {
		return backend.Result{}, errors.New("compute exec response omitted exit_code")
	}

	return backend.Result{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: *result.ExitCode}, nil
}
