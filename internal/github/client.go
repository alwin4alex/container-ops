// Package github reads required-env files from repos and triggers the
// deploy/teardown workflow through workflow_dispatch.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrNoToken = errors.New("GitHub token is not configured")

type Client struct {
	api   string
	token string
	http  *http.Client
}

func New(apiURL, token string, timeout time.Duration) *Client {
	return &Client{api: strings.TrimRight(apiURL, "/"), token: token, http: &http.Client{Timeout: timeout}}
}

func (c *Client) Enabled() bool { return c.token != "" }

func (c *Client) do(ctx context.Context, method, path string, body any, accept string) (*http.Response, error) {
	if c.token == "" {
		return nil, ErrNoToken
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var m struct{ Message string }
	_ = json.Unmarshal(b, &m)
	if m.Message == "" {
		m.Message = strings.TrimSpace(string(b))
	}
	return fmt.Errorf("github: %s: %s", resp.Status, m.Message)
}

// FileContent returns the raw content of a file at a ref.
func (c *Client) FileContent(ctx context.Context, repo, path, ref string) (string, error) {
	p := repoPath(repo) + "/contents/" + escapePath(path) + "?ref=" + url.QueryEscape(ref)
	resp, err := c.do(ctx, http.MethodGet, p, nil, "application/vnd.github.raw+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

func escapePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// Dispatch starts a workflow_dispatch run. GitHub returns no run id, so the
// caller finds the run through Runs afterwards.
func (c *Client) Dispatch(ctx context.Context, repo, workflow, ref string, inputs map[string]string) error {
	p := repoPath(repo) + "/actions/workflows/" + url.PathEscape(workflow) + "/dispatches"
	resp, err := c.do(ctx, http.MethodPost, p, map[string]any{"ref": ref, "inputs": inputs}, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}

type Run struct {
	ID           int64     `json:"id"`
	RunNumber    int       `json:"run_number"`
	DisplayTitle string    `json:"display_title"`
	Status       string    `json:"status"`     // queued | in_progress | completed | waiting ...
	Conclusion   string    `json:"conclusion"` // success | failure | cancelled ...
	HTMLURL      string    `json:"html_url"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	CreatedAt    time.Time `json:"created_at"`
	Actor        struct {
		Login string `json:"login"`
	} `json:"actor"`
}

// Runs lists recent runs of a workflow.
func (c *Client) Runs(ctx context.Context, repo, workflow string, n int) ([]Run, error) {
	p := fmt.Sprintf("%s/actions/workflows/%s/runs?per_page=%d", repoPath(repo), url.PathEscape(workflow), n)
	resp, err := c.do(ctx, http.MethodGet, p, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out struct {
		WorkflowRuns []Run `json:"workflow_runs"`
	}
	return out.WorkflowRuns, json.NewDecoder(resp.Body).Decode(&out)
}
