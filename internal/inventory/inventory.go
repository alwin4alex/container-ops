// Package inventory polls every project/environment endpoint and keeps the
// last known state, so a down endpoint still shows its last snapshot.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"container-ops/internal/config"
	"container-ops/internal/dockerapi"
	"container-ops/internal/envcheck"
	"container-ops/internal/github"
	"container-ops/internal/harbor"
)

type Key struct{ Project, Env string }

type Snapshot struct {
	Key
	Polled   bool // at least one poll attempted
	Up       bool
	LastSeen time.Time // last successful poll
	LastErr  string
	Engine   string
	Nodes    int
	NodesUp  int
	CPU      int
	MemBytes int64
	Images   int
	Volumes  int
	Services []Service
}

type Service struct {
	ID        string
	Name      string // without the stack prefix
	FullName  string
	Image     string
	Tag       string
	Digest    string
	Built     time.Time
	Desired   int
	Running   int
	Global    bool
	Env       envcheck.Result
	EnvNote   string // why the env check is unknown, if it is
	Update    string // swarm update state, e.g. updating, rollback_completed
	UpdateMsg string
	UpdatedAt time.Time
	DeployRun string // label deploy.run set by the deploy workflow
	DeployRef string // label deploy.commit
	Tasks     []Task
}

type Task struct {
	ID    string
	Slot  int
	Node  string
	State string
	Err   string
}

// Healthy is true when every desired replica is running.
func (s Service) Healthy() bool {
	if s.Global {
		return s.Running > 0 && s.Running >= len(s.Tasks)
	}
	return s.Running >= s.Desired
}

type requiredEntry struct {
	names   []string
	err     error
	fetched time.Time
}

type Inventory struct {
	cfg     *config.Config
	docker  map[Key]*dockerapi.Client
	gh      *github.Client
	harbor  *harbor.Client
	mu      sync.RWMutex
	snaps   map[Key]*Snapshot
	reqMu   sync.Mutex
	req     map[string]requiredEntry
	reqTTL  time.Duration
	timeout time.Duration
}

func New(cfg *config.Config, gh *github.Client, hb *harbor.Client) (*Inventory, error) {
	inv := &Inventory{
		cfg: cfg, gh: gh, harbor: hb,
		docker: map[Key]*dockerapi.Client{}, snaps: map[Key]*Snapshot{},
		req: map[string]requiredEntry{}, reqTTL: 5 * time.Minute, timeout: cfg.Server.RequestTimeout,
	}
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		for envID, ep := range p.Endpoints {
			c, err := dockerapi.New(ep.Docker, cfg.Server.RequestTimeout, dockerapi.TLSFiles{CA: ep.TLSCA, Cert: ep.TLSCert, Key: ep.TLSKey})
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", p.ID, envID, err)
			}
			k := Key{p.ID, envID}
			inv.docker[k] = c
			inv.snaps[k] = &Snapshot{Key: k}
		}
	}
	return inv, nil
}

// Run polls all endpoints now and then every refresh interval until ctx ends.
func (inv *Inventory) Run(ctx context.Context) {
	inv.RefreshAll(ctx)
	t := time.NewTicker(inv.cfg.Server.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			inv.RefreshAll(ctx)
		}
	}
}

func (inv *Inventory) RefreshAll(ctx context.Context) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for k := range inv.docker {
		wg.Add(1)
		sem <- struct{}{}
		go func(k Key) {
			defer wg.Done()
			defer func() { <-sem }()
			inv.Refresh(ctx, k)
		}(k)
	}
	wg.Wait()
}

// Refresh polls one endpoint. On failure the previous services are kept and
// the snapshot is marked down.
func (inv *Inventory) Refresh(ctx context.Context, k Key) {
	c := inv.docker[k]
	p, _ := inv.cfg.Project(k.Project)
	if c == nil || p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, inv.timeout*3)
	defer cancel()
	next, err := inv.collect(ctx, c, p)

	inv.mu.Lock()
	defer inv.mu.Unlock()
	prev := inv.snaps[k]
	if err != nil {
		prev.Polled, prev.Up, prev.LastErr = true, false, err.Error()
		slog.Warn("endpoint poll failed", "project", k.Project, "env", k.Env, "err", err)
		return
	}
	next.Key, next.Polled, next.Up, next.LastSeen = k, true, true, time.Now()
	inv.snaps[k] = next
}

func (inv *Inventory) collect(ctx context.Context, c *dockerapi.Client, p *config.Project) (*Snapshot, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Engine: engineLabel(info), Nodes: info.Swarm.Nodes, CPU: info.NCPU, MemBytes: info.MemTotal, Images: info.Images}
	if info.Swarm.LocalNodeState == "active" && !info.Swarm.ControlAvailable {
		return nil, errors.New("endpoint is a Swarm worker; point it at a manager")
	}
	services, err := c.StackServices(ctx, p.Stack)
	if err != nil {
		return nil, err
	}
	tasks, err := c.RunningTasks(ctx)
	if err != nil {
		return nil, err
	}
	nodeNames := map[string]string{}
	if nodes, err := c.Nodes(ctx); err == nil {
		s.Nodes = len(nodes)
		for _, n := range nodes {
			nodeNames[n.ID] = n.Description.Hostname
			if n.Status.State == "ready" {
				s.NodesUp++
			}
		}
	}
	if n, err := c.VolumeCount(ctx); err == nil {
		s.Volumes = n
	}

	byService := map[string][]dockerapi.Task{}
	for _, t := range tasks {
		byService[t.ServiceID] = append(byService[t.ServiceID], t)
	}
	for _, svc := range services {
		s.Services = append(s.Services, inv.serviceState(ctx, p, svc, byService[svc.ID], nodeNames))
	}
	sort.Slice(s.Services, func(i, j int) bool { return s.Services[i].Name < s.Services[j].Name })
	return s, nil
}

func engineLabel(i dockerapi.Info) string {
	if i.Swarm.LocalNodeState == "active" {
		return "Swarm " + i.ServerVersion
	}
	return "Standalone " + i.ServerVersion
}

func (inv *Inventory) serviceState(ctx context.Context, p *config.Project, svc dockerapi.Service, tasks []dockerapi.Task, nodes map[string]string) Service {
	cs := svc.Spec.TaskTemplate.ContainerSpec
	ref := harbor.ParseImage(cs.Image)
	short := strings.TrimPrefix(svc.Spec.Name, p.Stack+"_")
	out := Service{
		ID: svc.ID, Name: short, FullName: svc.Spec.Name,
		Image: strings.SplitN(cs.Image, "@", 2)[0], Tag: ref.Tag, Digest: ref.Digest,
		Desired: svc.Replicas(), UpdatedAt: svc.UpdatedAt,
		DeployRun: svc.Spec.Labels["deploy.run"], DeployRef: svc.Spec.Labels["deploy.commit"],
	}
	if out.Desired < 0 {
		out.Global, out.Desired = true, len(tasks)
	}
	if svc.UpdateStatus != nil {
		out.Update, out.UpdateMsg = svc.UpdateStatus.State, svc.UpdateStatus.Message
	}
	for _, t := range tasks {
		if t.Status.State == "running" {
			out.Running++
		}
		node := nodes[t.NodeID]
		if node == "" && len(t.NodeID) >= 12 {
			node = t.NodeID[:12]
		}
		out.Tasks = append(out.Tasks, Task{ID: t.ID, Slot: t.Slot, Node: node, State: t.Status.State, Err: t.Status.Err})
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Slot < out.Tasks[j].Slot })

	if inv.harbor != nil {
		if bt, ok, err := inv.harbor.BuildTime(ctx, ref); err == nil && ok {
			out.Built = bt
		}
	}

	// Env check: presence only, values are dropped here.
	present := envcheck.Presence(cs.Env)
	names, err := inv.required(ctx, p, short)
	if err != nil {
		out.EnvNote = err.Error()
	}
	out.Env = envcheck.Check(names, present, err == nil)
	return out
}

// required returns the required variable names for a service, cached.
func (inv *Inventory) required(ctx context.Context, p *config.Project, service string) ([]string, error) {
	if p.Repo == "" {
		return nil, errors.New("project has no repo configured")
	}
	if !inv.gh.Enabled() {
		return nil, github.ErrNoToken
	}
	path := p.RequiredEnvPath(service)
	key := p.Repo + "@" + p.Ref + ":" + path
	inv.reqMu.Lock()
	e, ok := inv.req[key]
	inv.reqMu.Unlock()
	if ok && time.Since(e.fetched) < inv.reqTTL {
		return e.names, e.err
	}
	content, err := inv.gh.FileContent(ctx, p.Repo, path, p.Ref)
	e = requiredEntry{fetched: time.Now(), err: err}
	if err == nil {
		e.names = envcheck.ParseExample(content)
	} else {
		e.err = fmt.Errorf("required list %s not loaded: %w", path, err)
	}
	inv.reqMu.Lock()
	inv.req[key] = e
	inv.reqMu.Unlock()
	return e.names, e.err
}

// Snapshot returns a copy of the current state of one endpoint.
func (inv *Inventory) Snapshot(k Key) (Snapshot, bool) {
	inv.mu.RLock()
	defer inv.mu.RUnlock()
	s, ok := inv.snaps[k]
	if !ok {
		return Snapshot{}, false
	}
	return *s, true
}

// Logs opens a service's logs. Task ids in log lines can be resolved to
// "slot@node" with the returned map.
func (inv *Inventory) Logs(ctx context.Context, k Key, service string, tail int, follow bool) (io.ReadCloser, map[string]string, error) {
	snap, ok := inv.Snapshot(k)
	if !ok {
		return nil, nil, errors.New("unknown project or environment")
	}
	for _, s := range snap.Services {
		if s.Name == service {
			names := map[string]string{}
			for _, t := range s.Tasks {
				names[t.ID] = fmt.Sprintf(".%d@%s", t.Slot, t.Node)
			}
			rc, err := inv.docker[k].ServiceLogs(ctx, s.ID, tail, follow)
			return rc, names, err
		}
	}
	return nil, nil, errors.New("service not found")
}
