package web

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"container-ops/internal/config"
	"container-ops/internal/envcheck"
	"container-ops/internal/inventory"
)

const timeFmt = "2006-01-02 15:04:05"

// ---- Severity, shared by cards, tabs and rows ----

type Sev string

const (
	SevOK   Sev = "ok"
	SevWarn Sev = "warn"
	SevBad  Sev = "bad"
)

func worse(a, b Sev) Sev {
	rank := map[Sev]int{SevOK: 0, SevWarn: 1, SevBad: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func serviceSev(s inventory.Service) Sev {
	switch {
	case !s.Healthy() || s.Env.Missing > 0:
		return SevBad
	case s.Env.Empty > 0 || deployState(s) != "Deployed":
		return SevWarn
	}
	return SevOK
}

// deployState turns Swarm's update status into a short label.
func deployState(s inventory.Service) string {
	switch s.Update {
	case "", "completed":
		return "Deployed"
	case "updating":
		return "Updating"
	case "paused":
		return "Update paused"
	case "rollback_started":
		return "Rolling back"
	case "rollback_paused":
		return "Rollback paused"
	case "rollback_completed":
		return "Rolled back"
	}
	return s.Update
}

// ---- Projects page ----

type ProjectsPage struct {
	Cards   []ProjectCard
	Summary string
	Query   string
	SelfURL string
}

type EnvPill struct {
	ID, Label, State, Endpoint, URL string
	Deployed, Up                    bool
	Dot                             string
}

type ProjectCard struct {
	ID, Name, Group           string
	Status, StatusClass, Icon string
	Sev                       Sev
	LastSeen, Engine          string
	Envs                      []EnvPill
	Stacks, Services          string
	Containers                string
	Running, Stopped          int
	Healthy, Unhealthy        int
	Volumes, Images           string
	CPU, RAM, Nodes           string
	URL, DeployURL            string
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

func projectURL(id, env, filter string) string {
	q := url.Values{}
	if env != "" {
		q.Set("env", env)
	}
	if filter != "" && filter != "all" {
		q.Set("filter", filter)
	}
	u := "/projects/" + url.PathEscape(id)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

type projectStatus struct{ Label, Class, Icon string }

func statusOf(snaps []inventory.Snapshot) projectStatus {
	down, polled := 0, 0
	for _, s := range snaps {
		if s.Polled {
			polled++
			if !s.Up {
				down++
			}
		}
	}
	switch {
	case polled == 0:
		return projectStatus{"Checking", "chip-neutral", "icon-neutral"}
	case down == 0:
		return projectStatus{"Up", "chip-ok", "icon-up"}
	case down == len(snaps):
		return projectStatus{"Down", "chip-bad", "icon-bad"}
	}
	return projectStatus{"Partly down", "chip-warn", "icon-warn"}
}

func (s *Server) projectsPage(query string) ProjectsPage {
	page := ProjectsPage{Query: query, SelfURL: "/"}
	if query != "" {
		page.SelfURL = "/?" + url.Values{"q": {query}}.Encode()
	}
	totalEp, upEp := 0, 0
	q := strings.ToLower(strings.TrimSpace(query))
	for i := range s.cfg.Projects {
		p := &s.cfg.Projects[i]
		envIDs := s.cfg.EnvIDs(p)
		var snaps []inventory.Snapshot
		for _, id := range envIDs {
			snap, _ := s.inv.Snapshot(inventory.Key{Project: p.ID, Env: id})
			snaps = append(snaps, snap)
		}
		card := s.projectCard(p, envIDs, snaps)
		for _, e := range card.Envs {
			if e.Deployed {
				totalEp++
				if e.Up {
					upEp++
				}
			}
		}
		if q != "" {
			hay := strings.ToLower(p.ID + " " + p.Name + " " + p.Group)
			for _, e := range card.Envs {
				hay += " " + e.Endpoint
			}
			if !strings.Contains(hay, q) {
				continue
			}
		}
		page.Cards = append(page.Cards, card)
	}
	page.Summary = fmt.Sprintf("%s · %s · %d/%d endpoints up",
		plural(len(s.cfg.Projects), "project"), plural(len(s.cfg.Environments), "environment"), upEp, totalEp)
	return page
}

func (s *Server) projectCard(p *config.Project, envIDs []string, snaps []inventory.Snapshot) ProjectCard {
	st := statusOf(snaps)
	c := ProjectCard{
		ID: p.ID, Name: p.Name, Group: p.Group, Status: st.Label, StatusClass: st.Class, Icon: st.Icon,
		Sev: SevOK, URL: projectURL(p.ID, "", ""), DeployURL: "/deploy?project=" + url.QueryEscape(p.ID),
	}
	if st.Label == "Down" || st.Label == "Partly down" {
		c.Sev = SevBad
	}
	var services, containers, volumes, images, cpu, nodes int
	var mem int64
	var lastSeen time.Time
	engines := map[string]bool{}
	var engineList []string
	snapBy := map[string]inventory.Snapshot{}
	for i, id := range envIDs {
		snapBy[id] = snaps[i]
	}
	for _, env := range s.cfg.Environments {
		ep, deployed := p.Endpoints[env.ID]
		pill := EnvPill{ID: env.ID, Label: env.Label, Deployed: deployed, State: "Not deployed", Dot: "dot-none"}
		if deployed {
			snap := snapBy[env.ID]
			pill.Endpoint = endpointHost(ep.Docker)
			pill.URL = projectURL(p.ID, env.ID, "")
			switch {
			case !snap.Polled:
				pill.State, pill.Dot = "Checking", "dot-none"
			case snap.Up:
				pill.State, pill.Dot, pill.Up = "Up", "dot-ok", true
			default:
				pill.State, pill.Dot = "Down", "dot-bad"
			}
			if snap.LastSeen.After(lastSeen) {
				lastSeen = snap.LastSeen
			}
			if snap.Engine != "" && !engines[snap.Engine] {
				engines[snap.Engine] = true
				engineList = append(engineList, snap.Engine)
			}
			volumes += snap.Volumes
			images += snap.Images
			cpu += snap.CPU
			mem += snap.MemBytes
			nodes += snap.Nodes
			for _, svc := range snap.Services {
				services++
				containers += svc.Desired
				c.Running += svc.Running
				if svc.Env.Issues() > 0 {
					c.Unhealthy += svc.Running
				}
				c.Sev = worse(c.Sev, serviceSev(svc))
			}
		}
		c.Envs = append(c.Envs, pill)
	}
	c.Stopped = max(containers-c.Running, 0)
	c.Healthy = c.Running - c.Unhealthy
	c.Stacks = plural(len(envIDs), "stack")
	c.Services = plural(services, "service")
	c.Containers = plural(containers, "container")
	c.Volumes = plural(volumes, "volume")
	c.Images = plural(images, "image")
	c.CPU = fmt.Sprintf("%d CPU", cpu)
	c.RAM = fmt.Sprintf("%.1f GB RAM", float64(mem)/(1<<30))
	c.Nodes = plural(nodes, "node")
	c.Engine = strings.Join(engineList, " / ")
	if lastSeen.IsZero() {
		c.LastSeen = "never reached"
	} else {
		c.LastSeen = lastSeen.Local().Format(timeFmt)
	}
	return c
}

// ---- Project (containers) page ----

type EnvTab struct {
	ID, Label, Sub, SubClass, Dot, URL string
	Active, Deployed                   bool
}

type Infra struct {
	Endpoint, Swarm, Engine, Nodes, CPU, RAM, Volumes, Images, LastSeen string
	Down, NeverPolled                                                   bool
	DownText                                                            string
}

type KPI struct {
	Label, Value, Sub, ValueClass, URL string
	Active                             bool
}

type Filter struct {
	Label, URL string
	Count      int
	Active     bool
}

type Row struct {
	Name, Image, Digest, DigestShort, Built string
	Replicas, HealthClass                   string
	EnvLabel, EnvClass                      string
	DeployLabel, DeployClass, DeployWhen    string
	Sev                                     Sev
	DrawerURL, LogsURL                      string
}

type ProjectPage struct {
	ID, Name, Status, StatusClass, Sub string
	Env, EnvLabel                      string
	Tabs                               []EnvTab
	Infra                              Infra
	KPIs                               []KPI
	Filters                            []Filter
	Rows                               []Row
	TotalServices                      int
	Filter                             string
	SelfURL                            string
}

func envLabelFor(r envcheck.Result) (string, string) {
	switch {
	case !r.Known:
		return "Unknown", "chip-neutral"
	case r.Missing > 0 && r.Empty > 0:
		return fmt.Sprintf("%d missing, %d empty", r.Missing, r.Empty), "chip-bad"
	case r.Missing > 0:
		return fmt.Sprintf("%d missing", r.Missing), "chip-bad"
	case r.Empty > 0:
		return fmt.Sprintf("%d empty", r.Empty), "chip-warn"
	}
	return fmt.Sprintf("%d/%d OK", r.Required, r.Required), "chip-ok"
}

func replicasLabel(s inventory.Service) (string, string) {
	if s.Healthy() {
		return fmt.Sprintf("%d/%d running", s.Running, s.Desired), "chip-ok"
	}
	return fmt.Sprintf("%d/%d running", s.Running, s.Desired), "chip-bad"
}

func shortDigest(d string) string {
	if len(d) > 19 { // "sha256:" + 12
		return d[:19]
	}
	if d == "" {
		return "no digest pinned"
	}
	return d
}

func (s *Server) imageNoRegistry(image string) string {
	if s.cfg.Harbor.URL != "" {
		if u, err := url.Parse(s.cfg.Harbor.URL); err == nil {
			return strings.TrimPrefix(image, u.Host+"/")
		}
	}
	return image
}

var filterTests = map[string]func(inventory.Service) bool{
	"all":  func(inventory.Service) bool { return true },
	"env":  func(s inventory.Service) bool { return s.Env.Issues() > 0 },
	"down": func(s inventory.Service) bool { return !s.Healthy() || deployState(s) != "Deployed" },
}

func (s *Server) projectPage(p *config.Project, envID, filter string) ProjectPage {
	if _, ok := filterTests[filter]; !ok {
		filter = "all"
	}
	st := statusOf(s.snapsFor(p))
	page := ProjectPage{
		ID: p.ID, Name: p.Name, Status: st.Label, StatusClass: st.Class, Env: envID, Filter: filter,
		Sub:     fmt.Sprintf("stack %s · repo %s", p.Stack, orDash(p.Repo)),
		SelfURL: projectURL(p.ID, envID, filter),
	}
	for _, env := range s.cfg.Environments {
		tab := EnvTab{ID: env.ID, Label: env.Label, Active: env.ID == envID, Dot: "dot-none", SubClass: "muted"}
		if env.ID == envID {
			page.EnvLabel = env.Label
		}
		if _, ok := p.Endpoints[env.ID]; !ok {
			tab.Sub = "Not deployed"
			page.Tabs = append(page.Tabs, tab)
			continue
		}
		tab.Deployed, tab.URL = true, projectURL(p.ID, env.ID, "")
		snap, _ := s.inv.Snapshot(inventory.Key{Project: p.ID, Env: env.ID})
		up, total, issues := 0, 0, 0
		for _, svc := range snap.Services {
			up += svc.Running
			total += svc.Desired
			if serviceSev(svc) != SevOK {
				issues++
			}
		}
		switch {
		case !snap.Polled:
			tab.Sub = "Checking…"
		case !snap.Up:
			tab.Dot, tab.SubClass = "dot-bad", "text-bad"
			tab.Sub = "Down"
			if !snap.LastSeen.IsZero() {
				tab.Sub += " · last seen " + snap.LastSeen.Local().Format("2006-01-02")
			}
		default:
			tab.Dot = "dot-ok"
			if issues > 0 {
				tab.Dot = "dot-warn"
			}
			tab.Sub = fmt.Sprintf("%d/%d containers up", up, total)
			if issues > 0 {
				tab.Sub += fmt.Sprintf(" · %d issues", issues)
			}
		}
		page.Tabs = append(page.Tabs, tab)
	}

	ep := p.Endpoints[envID]
	snap, _ := s.inv.Snapshot(inventory.Key{Project: p.ID, Env: envID})
	page.Infra = Infra{
		Endpoint: endpointHost(ep.Docker), Swarm: ep.Swarm, Engine: orDash(snap.Engine),
		Nodes: fmt.Sprintf("%s (%d ready)", plural(snap.Nodes, "node"), snap.NodesUp),
		CPU:   fmt.Sprintf("%d CPU", snap.CPU), RAM: fmt.Sprintf("%.1f GB RAM", float64(snap.MemBytes)/(1<<30)),
		Volumes: plural(snap.Volumes, "volume"), Images: plural(snap.Images, "image"),
		LastSeen: "never", NeverPolled: !snap.Polled,
	}
	if !snap.LastSeen.IsZero() {
		page.Infra.LastSeen = snap.LastSeen.Local().Format(timeFmt)
	}
	if snap.Polled && !snap.Up {
		page.Infra.Down = true
		if snap.LastSeen.IsZero() {
			page.Infra.DownText = fmt.Sprintf("Endpoint %s cannot be reached (%s). No data has been collected yet.", page.Infra.Endpoint, snap.LastErr)
		} else {
			page.Infra.DownText = fmt.Sprintf("Endpoint %s has been unreachable since %s (%s). The containers below are the last known state, not live.", page.Infra.Endpoint, page.Infra.LastSeen, snap.LastErr)
		}
	}

	// KPIs
	running, envVars, envSvc, deployOK := 0, 0, 0, 0
	for _, svc := range snap.Services {
		if svc.Healthy() {
			running++
		}
		if svc.Env.Issues() > 0 {
			envVars += svc.Env.Issues()
			envSvc++
		}
		if deployState(svc) == "Deployed" {
			deployOK++
		}
	}
	n := len(snap.Services)
	page.TotalServices = n
	kpi := func(label, value, sub, cls, f string) KPI {
		k := KPI{Label: label, Value: value, Sub: sub, ValueClass: cls, Active: filter == f && f != "all"}
		target := f
		if k.Active {
			target = "all"
		}
		k.URL = projectURL(p.ID, envID, target)
		return k
	}
	runSub := "All replicas up"
	if n-running > 0 {
		runSub = fmt.Sprintf("%d not fully running", n-running)
	}
	envSub := "All required vars present"
	if envVars > 0 {
		envSub = fmt.Sprintf("in %s · missing or empty", plural(envSvc, "service"))
	}
	depSub := "All updates completed"
	if n-deployOK > 0 {
		depSub = fmt.Sprintf("%d updating, paused or rolled back", n-deployOK)
	}
	page.KPIs = []KPI{
		kpi("Services running", fmt.Sprintf("%d / %d", running, n), runSub, classIf(running < n, "text-bad"), "down"),
		kpi("Env var issues", fmt.Sprint(envVars), envSub, classIf(envVars > 0, "text-bad"), "env"),
		kpi("Last deploy OK", fmt.Sprintf("%d / %d", deployOK, n), depSub, classIf(deployOK < n, "text-warn"), "down"),
	}
	page.KPIs[2].Active = false

	for _, f := range []struct{ id, label string }{{"all", "All"}, {"env", "Env issues"}, {"down", "Failing"}} {
		cnt := 0
		for _, svc := range snap.Services {
			if filterTests[f.id](svc) {
				cnt++
			}
		}
		page.Filters = append(page.Filters, Filter{Label: f.label, Count: cnt, Active: filter == f.id, URL: projectURL(p.ID, envID, f.id)})
	}

	base := "/projects/" + url.PathEscape(p.ID) + "/envs/" + url.PathEscape(envID) + "/services/"
	for _, svc := range snap.Services {
		if !filterTests[filter](svc) {
			continue
		}
		rep, repCls := replicasLabel(svc)
		envLbl, envCls := envLabelFor(svc.Env)
		dl := deployState(svc)
		dcls := "chip-ok"
		if dl != "Deployed" {
			dcls = "chip-warn"
		}
		built := "build date unknown"
		if !svc.Built.IsZero() {
			built = "Built " + svc.Built.Local().Format("Jan 2, 2006 · 15:04")
		}
		when := ""
		if !svc.UpdatedAt.IsZero() {
			when = svc.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		if svc.DeployRun != "" {
			when += " · run " + svc.DeployRun
		}
		page.Rows = append(page.Rows, Row{
			Name: svc.Name, Image: s.imageNoRegistry(svc.Image), Digest: orDash(svc.Digest), DigestShort: shortDigest(svc.Digest), Built: built,
			Replicas: rep, HealthClass: repCls, EnvLabel: envLbl, EnvClass: envCls,
			DeployLabel: dl, DeployClass: dcls, DeployWhen: when, Sev: serviceSev(svc),
			DrawerURL: base + url.PathEscape(svc.Name) + "?tab=env",
			LogsURL:   base + url.PathEscape(svc.Name) + "?tab=logs",
		})
	}
	return page
}

func (s *Server) snapsFor(p *config.Project) []inventory.Snapshot {
	var out []inventory.Snapshot
	for _, id := range s.cfg.EnvIDs(p) {
		snap, _ := s.inv.Snapshot(inventory.Key{Project: p.ID, Env: id})
		out = append(out, snap)
	}
	return out
}

func classIf(cond bool, cls string) string {
	if cond {
		return cls
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---- Drawer ----

type EnvRow struct{ Name, Status, Class string }

type Drawer struct {
	Name, Scope, Image, Digest, Built string
	Replicas, HealthClass             string
	EnvLabel, EnvClass                string
	DeployLabel, DeployClass          string
	EnvSummary, EnvNote               string
	EnvRows                           []EnvRow
	Tab, Sev                          string
	EnvURL, LogsURL, StreamURL        string
	RepoFileURL                       string
	Tasks                             []TaskChip
	Lines                             []LogLine
	LogErr                            string
	LogTail                           int
}

type TaskChip struct{ ID, Label string }

type LogLine struct {
	Time, Task, Level, Msg string
}

func (s *Server) drawer(p *config.Project, envID string, svc inventory.Service, tab string) Drawer {
	env, _ := s.cfg.Env(envID)
	rep, repCls := replicasLabel(svc)
	envLbl, envCls := envLabelFor(svc.Env)
	dl := deployState(svc)
	dcls := "chip-ok"
	if dl != "Deployed" {
		dcls = "chip-warn"
	}
	base := "/projects/" + url.PathEscape(p.ID) + "/envs/" + url.PathEscape(envID) + "/services/" + url.PathEscape(svc.Name)
	d := Drawer{
		Name: svc.Name, Scope: fmt.Sprintf("%s · %s · %s", p.Name, env.Label, p.Endpoints[envID].Swarm),
		Image: svc.Image, Digest: orDash(svc.Digest), Built: "unknown",
		Replicas: rep, HealthClass: repCls, EnvLabel: envLbl, EnvClass: envCls, DeployLabel: dl, DeployClass: dcls,
		Tab: tab, Sev: string(serviceSev(svc)), EnvNote: svc.EnvNote,
		EnvURL: base + "?tab=env", LogsURL: base + "?tab=logs", StreamURL: base + "/logs/stream",
		LogTail: s.cfg.Server.LogTail,
	}
	if !svc.Built.IsZero() {
		d.Built = svc.Built.Local().Format(timeFmt)
	}
	if p.Repo != "" {
		d.RepoFileURL = "https://github.com/" + p.Repo + "/blob/" + url.PathEscape(p.Ref) + "/" + strings.TrimPrefix(p.RequiredEnvPath(svc.Name), "/")
	}
	switch {
	case !svc.Env.Known:
		d.EnvSummary = "Required list not loaded"
	case svc.Env.Issues() > 0:
		d.EnvSummary = fmt.Sprintf("%s of %d", envLbl, svc.Env.Required)
	default:
		d.EnvSummary = fmt.Sprintf("All %d present", svc.Env.Required)
	}
	for _, v := range svc.Env.Vars {
		row := EnvRow{Name: v.Name}
		switch v.Status {
		case envcheck.Missing:
			row.Status, row.Class = "Missing", "chip-bad"
		case envcheck.Empty:
			row.Status, row.Class = "Empty", "chip-warn"
		default:
			row.Status, row.Class = "OK", "chip-ok"
		}
		d.EnvRows = append(d.EnvRows, row)
	}
	tasks := append([]inventory.Task(nil), svc.Tasks...)
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Slot < tasks[j].Slot })
	for _, t := range tasks {
		d.Tasks = append(d.Tasks, TaskChip{ID: fmt.Sprintf(".%d@%s", t.Slot, t.Node), Label: fmt.Sprintf(".%d %s", t.Slot, t.Node)})
	}
	return d
}
