// Package dockerapi is a small read-only client for the Docker Engine API.
// It covers only what the dashboard reads, so it has no dependency on the
// Docker SDK and works through a read-only socket proxy.
package dockerapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	base string
	http *http.Client
	// Streaming requests (logs with follow) use a client without a timeout.
	stream *http.Client
}

type TLSFiles struct{ CA, Cert, Key string }

func New(baseURL string, timeout time.Duration, tf TLSFiles) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // endpoints are on the internal network
	if tf.CA != "" || tf.Cert != "" {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if tf.CA != "" {
			pem, err := os.ReadFile(tf.CA)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no certificates in %s", tf.CA)
			}
			tc.RootCAs = pool
		}
		if tf.Cert != "" {
			cert, err := tls.LoadX509KeyPair(tf.Cert, tf.Key)
			if err != nil {
				return nil, err
			}
			tc.Certificates = []tls.Certificate{cert}
		}
		tr.TLSClientConfig = tc
	}
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		http:   &http.Client{Transport: tr, Timeout: timeout},
		stream: &http.Client{Transport: tr},
	}, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func filters(f map[string][]string) url.Values {
	b, _ := json.Marshal(f)
	return url.Values{"filters": {string(b)}}
}

// ---- Types (only the fields the dashboard uses) ----

type Info struct {
	ServerVersion string
	NCPU          int
	MemTotal      int64
	Images        int
	Swarm         struct {
		LocalNodeState   string
		ControlAvailable bool
		Nodes            int
		Managers         int
	}
}

type Service struct {
	ID   string
	Spec struct {
		Name         string
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct {
				Image  string
				Env    []string
				Labels map[string]string
			}
		}
		Mode struct {
			Replicated *struct{ Replicas *uint64 }
			Global     *struct{}
		}
	}
	UpdatedAt    time.Time
	UpdateStatus *struct {
		State   string
		Message string
	}
}

type Task struct {
	ID        string
	ServiceID string
	NodeID    string
	Slot      int
	Status    struct {
		State   string
		Message string
		Err     string
	}
	DesiredState string
}

type Volume struct{ Name string }

// ---- Calls ----

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, c.getJSON(ctx, "/info", nil, &i)
}

// StackServices lists the services that belong to a stack namespace.
func (c *Client) StackServices(ctx context.Context, stack string) ([]Service, error) {
	var s []Service
	q := filters(map[string][]string{"label": {"com.docker.stack.namespace=" + stack}})
	return s, c.getJSON(ctx, "/services", q, &s)
}

// RunningTasks returns tasks whose desired state is running, for all services.
func (c *Client) RunningTasks(ctx context.Context) ([]Task, error) {
	var t []Task
	q := filters(map[string][]string{"desired-state": {"running"}})
	return t, c.getJSON(ctx, "/tasks", q, &t)
}

func (c *Client) VolumeCount(ctx context.Context) (int, error) {
	var v struct{ Volumes []Volume }
	if err := c.getJSON(ctx, "/volumes", nil, &v); err != nil {
		return 0, err
	}
	return len(v.Volumes), nil
}

// ServiceLogs opens the log stream of a service. The caller must close it.
// The stream is multiplexed (see Demux) unless the service runs with a TTY.
func (c *Client) ServiceLogs(ctx context.Context, serviceID string, tail int, follow bool) (io.ReadCloser, error) {
	q := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"timestamps": {"1"},
		"details":    {"1"},
		"tail":       {fmt.Sprint(tail)},
	}
	if follow {
		q.Set("follow", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/services/"+url.PathEscape(serviceID)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	cl := c.http
	if follow {
		cl = c.stream
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("docker logs: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// Replicas returns desired replicas; global services report -1.
func (s Service) Replicas() int {
	if r := s.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
		return int(*r.Replicas)
	}
	if s.Spec.Mode.Global != nil {
		return -1
	}
	return 0
}

type Node struct {
	ID          string
	Description struct{ Hostname string }
	Status      struct{ State string }
	Spec        struct{ Role string }
}

func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var n []Node
	return n, c.getJSON(ctx, "/nodes", nil, &n)
}
