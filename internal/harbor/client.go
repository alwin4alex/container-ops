// Package harbor looks up when an image digest was built, using Harbor's
// artifact API. Results are cached per digest because they never change.
package harbor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Client struct {
	base       string
	host       string
	user, pass string
	http       *http.Client
	mu         sync.Mutex
	cache      map[string]time.Time
}

func New(baseURL, user, pass, caFile string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/"), host: u.Host, user: user, pass: pass,
		http: &http.Client{Transport: tr, Timeout: timeout}, cache: map[string]time.Time{},
	}, nil
}

// ImageRef is a parsed image reference such as
// harbor.local/bss/transfer:1.9.0@sha256:abc...
type ImageRef struct {
	Registry, Project, Repository, Tag, Digest string
}

func ParseImage(image string) ImageRef {
	var r ImageRef
	rest := image
	if i := strings.Index(rest, "@"); i >= 0 {
		r.Digest = rest[i+1:]
		rest = rest[:i]
	}
	// tag is after the last ":" that comes after the last "/"
	if slash := strings.LastIndex(rest, "/"); strings.LastIndex(rest, ":") > slash {
		c := strings.LastIndex(rest, ":")
		r.Tag = rest[c+1:]
		rest = rest[:c]
	}
	parts := strings.Split(rest, "/")
	if len(parts) >= 3 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		r.Registry = parts[0]
		r.Project = parts[1]
		r.Repository = strings.Join(parts[2:], "/")
	} else if len(parts) == 2 {
		r.Project, r.Repository = parts[0], parts[1]
	} else {
		r.Repository = rest
	}
	return r
}

// BuildTime returns the image build time for a digest stored in this Harbor.
// It prefers the OCI label org.opencontainers.image.created and falls back to
// the image config's created time. ok is false when the image is not in this
// Harbor or has no digest.
func (c *Client) BuildTime(ctx context.Context, ref ImageRef) (t time.Time, ok bool, err error) {
	if c == nil || ref.Digest == "" || ref.Registry != c.host || ref.Project == "" {
		return time.Time{}, false, nil
	}
	c.mu.Lock()
	if v, hit := c.cache[ref.Digest]; hit {
		c.mu.Unlock()
		return v, !v.IsZero(), nil
	}
	c.mu.Unlock()

	u := fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s/artifacts/%s",
		c.base, url.PathEscape(ref.Project),
		url.PathEscape(url.PathEscape(ref.Repository)), // Harbor wants "a/b" double-encoded
		url.PathEscape(ref.Digest))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return time.Time{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		c.store(ref.Digest, time.Time{})
		return time.Time{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, false, fmt.Errorf("harbor: %s", resp.Status)
	}
	var a struct {
		ExtraAttrs struct {
			Created string `json:"created"`
			Config  struct {
				Labels map[string]string `json:"Labels"`
			} `json:"config"`
		} `json:"extra_attrs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return time.Time{}, false, err
	}
	for _, s := range []string{a.ExtraAttrs.Config.Labels["org.opencontainers.image.created"], a.ExtraAttrs.Created} {
		if v, err := time.Parse(time.RFC3339Nano, s); err == nil {
			c.store(ref.Digest, v)
			return v, true, nil
		}
	}
	c.store(ref.Digest, time.Time{})
	return time.Time{}, false, nil
}

func (c *Client) store(d string, t time.Time) {
	c.mu.Lock()
	c.cache[d] = t
	c.mu.Unlock()
}
