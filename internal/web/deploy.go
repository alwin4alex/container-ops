package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"container-ops/internal/auth"
	"container-ops/internal/config"
	"container-ops/internal/github"
)

var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

type DeployEnv struct {
	ID, Label       string
	Deployed        bool
	RequireConfirm  bool
	RequireApproval bool
	Checked         bool
}

type ProjectOption struct {
	ID, Name, Repo string
	Selected       bool
}

type DeployPage struct {
	Projects      []ProjectOption
	Project       *config.Project
	Envs          []DeployEnv
	DefaultRef    string
	CanDeploy     bool
	GitHubEnabled bool
	CSRF          string
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.Projects) == 0 {
		http.Error(w, "no projects configured", http.StatusNotFound)
		return
	}
	p, ok := s.cfg.Project(r.URL.Query().Get("project"))
	if !ok {
		p = &s.cfg.Projects[0]
	}
	u := auth.FromContext(r.Context())
	page := DeployPage{
		Project: p, DefaultRef: p.Ref, CanDeploy: u.CanDeploy(), GitHubEnabled: s.gh.Enabled(), CSRF: s.auth.CSRFToken(u),
	}
	for _, q := range s.cfg.Projects {
		page.Projects = append(page.Projects, ProjectOption{ID: q.ID, Name: q.Name, Repo: q.Repo, Selected: q.ID == p.ID})
	}
	for i, e := range s.cfg.Environments {
		_, deployed := p.Endpoints[e.ID]
		page.Envs = append(page.Envs, DeployEnv{ID: e.ID, Label: e.Label, Deployed: deployed,
			RequireConfirm: e.RequireConfirm, RequireApproval: e.RequireApproval, Checked: i == 0})
	}
	s.render(w, r, "deploy", "Deployments", "deploy", page)
}

// ---- Recent runs ----

type RunView struct {
	Title, Sub, Label, Class, URL string
}

type RunsData struct {
	Project string
	Runs    []RunView
	Err     string
}

func runView(run github.Run) RunView {
	v := RunView{Title: run.DisplayTitle, URL: run.HTMLURL}
	v.Sub = fmt.Sprintf("#%d · %s · %s · %s", run.RunNumber, run.HeadBranch, run.Actor.Login, run.CreatedAt.Local().Format("2006-01-02 15:04"))
	switch {
	case run.Status == "waiting":
		v.Label, v.Class = "Awaiting approval", "chip-warn"
	case run.Status != "completed":
		v.Label, v.Class = strings.ReplaceAll(run.Status, "_", " "), "chip-info"
	case run.Conclusion == "success":
		v.Label, v.Class = "Succeeded", "chip-ok"
	case run.Conclusion == "cancelled" || run.Conclusion == "skipped":
		v.Label, v.Class = run.Conclusion, "chip-neutral"
	default:
		v.Label, v.Class = "Failed", "chip-bad"
	}
	return v
}

func (s *Server) projectRuns(ctx context.Context, p *config.Project, fresh bool) ([]github.Run, error) {
	s.runsMu.Lock()
	c, ok := s.runs[p.ID]
	s.runsMu.Unlock()
	if ok && !fresh && time.Since(c.at) < 15*time.Second {
		return c.runs, c.err
	}
	runs, err := s.gh.Runs(ctx, p.Repo, p.Workflow, 10)
	s.runsMu.Lock()
	s.runs[p.ID] = cachedRuns{at: time.Now(), runs: runs, err: err}
	s.runsMu.Unlock()
	return runs, err
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	p, ok := s.cfg.Project(r.URL.Query().Get("project"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	data := RunsData{Project: p.ID}
	if p.Repo == "" {
		data.Err = "This project has no repo configured."
	} else if runs, err := s.projectRuns(r.Context(), p, r.URL.Query().Get("fresh") == "1"); err != nil {
		data.Err = err.Error()
	} else {
		for _, run := range runs {
			data.Runs = append(data.Runs, runView(run))
		}
	}
	s.renderPartial(w, "runs", data)
}

// ---- Deploy / teardown ----

type deployRequest struct {
	Project  *config.Project
	Action   string
	Ref      string
	Envs     []config.Environment
	Skipped  []string // teardown targets where the project is not deployed
	NewEnvs  []string // deploy targets with no endpoint configured yet
	NeedType bool
}

type ConfirmData struct {
	Title, Body, Note, Word, CTA, Action, Ref, Project string
	Envs                                               []string
	NeedType, Teardown                                 bool
	Error                                              string
	CSRF                                               string
}

type ResultData struct {
	OK      bool
	Message string
	Project string
	Details []string
}

func (s *Server) parseDeploy(r *http.Request) (*deployRequest, error) {
	if !auth.FromContext(r.Context()).CanDeploy() {
		return nil, fmt.Errorf("your role cannot deploy or tear down")
	}
	if !s.auth.CheckCSRF(r) {
		return nil, fmt.Errorf("security token missing or expired, reload the page")
	}
	p, ok := s.cfg.Project(r.PostFormValue("project"))
	if !ok {
		return nil, fmt.Errorf("unknown project")
	}
	if p.Repo == "" {
		return nil, fmt.Errorf("project %s has no repo configured", p.Name)
	}
	req := &deployRequest{Project: p, Action: r.PostFormValue("action"), Ref: strings.TrimSpace(r.PostFormValue("ref"))}
	if req.Action != "deploy" && req.Action != "teardown" {
		return nil, fmt.Errorf("unknown action")
	}
	if req.Ref == "" {
		req.Ref = p.Ref
	}
	if !refPattern.MatchString(req.Ref) || strings.Contains(req.Ref, "..") {
		return nil, fmt.Errorf("ref %q is not a valid branch or tag", req.Ref)
	}
	chosen := map[string]bool{}
	for _, e := range r.PostForm["env"] {
		chosen[e] = true
	}
	for _, e := range s.cfg.Environments {
		if !chosen[e.ID] {
			continue
		}
		_, deployed := p.Endpoints[e.ID]
		if req.Action == "teardown" && !deployed {
			req.Skipped = append(req.Skipped, e.Label)
			continue
		}
		if !deployed {
			req.NewEnvs = append(req.NewEnvs, e.Label)
		}
		req.Envs = append(req.Envs, e)
		if e.RequireConfirm {
			req.NeedType = true
		}
	}
	if len(req.Envs) == 0 {
		return nil, fmt.Errorf("choose at least one environment where this action applies")
	}
	if req.Action == "teardown" {
		req.NeedType = true
	}
	return req, nil
}

func labels(envs []config.Environment) []string {
	var out []string
	for _, e := range envs {
		out = append(out, e.Label)
	}
	return out
}

func (s *Server) handleDeployConfirm(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseDeploy(r)
	if err != nil {
		s.renderPartial(w, "confirm", ConfirmData{Error: err.Error()})
		return
	}
	p := req.Project
	targets := strings.Join(labels(req.Envs), ", ")
	var swarms, approval []string
	for _, e := range req.Envs {
		if ep, ok := p.Endpoints[e.ID]; ok {
			swarms = append(swarms, ep.Swarm)
		}
		if e.RequireApproval {
			approval = append(approval, e.Label)
		}
	}
	d := ConfirmData{
		Project: p.ID, Action: req.Action, Ref: req.Ref, NeedType: req.NeedType, Word: p.ID,
		Teardown: req.Action == "teardown", CSRF: s.auth.CSRFToken(auth.FromContext(r.Context())),
	}
	for _, e := range req.Envs {
		d.Envs = append(d.Envs, e.ID)
	}
	if d.Teardown {
		d.Title = fmt.Sprintf("Tear down %s in %s?", p.Name, targets)
		d.Body = fmt.Sprintf("Runs docker stack rm %s on %s through the %s workflow. Volumes, secrets and the server .env are kept, so a redeploy restores the stack.", p.Stack, strings.Join(swarms, ", "), p.Workflow)
		d.CTA = "Tear down"
		if len(req.Skipped) > 0 {
			d.Note = fmt.Sprintf("%s is not deployed in %s — skipped.", p.Name, strings.Join(req.Skipped, ", "))
		}
	} else {
		d.Title = fmt.Sprintf("Deploy %s to %s?", p.Name, targets)
		d.Body = fmt.Sprintf("Runs the %s workflow in %s at ref %s: docker stack deploy with the compose file and server .env, then verifies env vars and replicas.", p.Workflow, p.Repo, req.Ref)
		if len(approval) > 0 {
			d.Body += fmt.Sprintf(" %s waits for a required reviewer to approve in GitHub.", strings.Join(approval, ", "))
		}
		d.CTA = "Deploy"
		if len(req.NewEnvs) > 0 {
			d.Note = fmt.Sprintf("First deploy of %s to %s — the runner there needs the server .env, and the dashboard needs an endpoint in its config to show it.", p.Name, strings.Join(req.NewEnvs, ", "))
		}
	}
	s.renderPartial(w, "confirm", d)
}

func (s *Server) handleDeployRun(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseDeploy(r)
	if err != nil {
		s.renderPartial(w, "result", ResultData{Message: err.Error()})
		return
	}
	if req.NeedType && strings.TrimSpace(r.PostFormValue("confirm_text")) != req.Project.ID {
		s.renderPartial(w, "result", ResultData{Project: req.Project.ID, Message: fmt.Sprintf("Type %s exactly to confirm.", req.Project.ID)})
		return
	}
	u := auth.FromContext(r.Context())
	res := ResultData{Project: req.Project.ID, OK: true}
	for _, e := range req.Envs {
		inputs := map[string]string{
			"action": req.Action, "environment": e.ID, "stack": req.Project.Stack, "requested_by": u.Name,
		}
		err := s.gh.Dispatch(r.Context(), req.Project.Repo, req.Project.Workflow, req.Ref, inputs)
		slog.Info("audit", "user", u.Name, "action", req.Action, "project", req.Project.ID, "env", e.ID, "ref", req.Ref, "ok", err == nil, "err", errString(err))
		if err != nil {
			res.OK = false
			res.Details = append(res.Details, fmt.Sprintf("%s: %v", e.Label, err))
			continue
		}
		res.Details = append(res.Details, fmt.Sprintf("%s: workflow started", e.Label))
	}
	verb := "Deploy"
	if req.Action == "teardown" {
		verb = "Teardown"
	}
	if res.OK {
		res.Message = fmt.Sprintf("%s of %s queued in GitHub Actions.", verb, req.Project.Name)
	} else {
		res.Message = fmt.Sprintf("%s of %s did not start for every environment.", verb, req.Project.Name)
	}
	s.runsMu.Lock()
	delete(s.runs, req.Project.ID)
	s.runsMu.Unlock()
	w.Header().Set("HX-Trigger", "refreshRuns")
	s.renderPartial(w, "result", res)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
