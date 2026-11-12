// Package web serves the dashboard: server-rendered pages with htmx for
// partial updates, plus a server-sent-events stream for following logs.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"container-ops/internal/auth"
	"container-ops/internal/config"
	"container-ops/internal/dockerapi"
	"container-ops/internal/github"
	"container-ops/internal/inventory"
	"container-ops/internal/logmask"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg      *config.Config
	inv      *inventory.Inventory
	gh       *github.Client
	auth     *auth.Authenticator
	pages    map[string]*template.Template
	partials *template.Template
	streams  chan struct{}

	runsMu sync.Mutex
	runs   map[string]cachedRuns
}

type cachedRuns struct {
	at   time.Time
	runs []github.Run
	err  error
}

func New(cfg *config.Config, inv *inventory.Inventory, gh *github.Client, a *auth.Authenticator) (*Server, error) {
	s := &Server{
		cfg: cfg, inv: inv, gh: gh, auth: a,
		pages:   map[string]*template.Template{},
		streams: make(chan struct{}, cfg.Server.MaxLogStreams),
		runs:    map[string]cachedRuns{},
	}
	funcs := template.FuncMap{"lower": strings.ToLower, "join": strings.Join}
	var err error
	s.partials, err = template.New("partials").Funcs(funcs).ParseFS(templateFS, "templates/partials.html")
	if err != nil {
		return nil, err
	}
	for _, page := range []string{"projects", "project", "deploy"} {
		t, err := template.New(page).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", s.handleProjects)
	app.HandleFunc("GET /projects", s.handleFirstProject)
	app.HandleFunc("GET /projects/{project}", s.handleProject)
	app.HandleFunc("GET /projects/{project}/envs/{env}/services/{service}", s.handleDrawer)
	app.HandleFunc("GET /projects/{project}/envs/{env}/services/{service}/logs/stream", s.handleLogStream)
	app.HandleFunc("GET /deploy", s.handleDeployPage)
	app.HandleFunc("GET /deploy/runs", s.handleRuns)
	app.HandleFunc("POST /deploy/confirm", s.handleDeployConfirm)
	app.HandleFunc("POST /deploy/run", s.handleDeployRun)
	mux.Handle("/", s.auth.Middleware(app))

	return securityHeaders(logRequests(mux))
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/static/") {
			slog.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
		}
	})
}

// ---- Rendering ----

type layoutData struct {
	Title, Active, CSRF, User string
	CanDeploy                 bool
	EnvSummary                string
	RefreshSeconds            int
	Page                      any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title, active string, data any) {
	u := auth.FromContext(r.Context())
	var labels []string
	for _, e := range s.cfg.Environments {
		labels = append(labels, e.Label)
	}
	ld := layoutData{
		Title: title, Active: active, CSRF: s.auth.CSRFToken(u), User: u.Name, CanDeploy: u.CanDeploy(),
		EnvSummary: strings.Join(labels, ", "), RefreshSeconds: int(s.cfg.Server.RefreshInterval.Seconds()), Page: data,
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", ld); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.partials.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("render partial", "name", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// ---- Pages ----

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "projects", "Projects", "projects", s.projectsPage(r.URL.Query().Get("q")))
}

func (s *Server) handleFirstProject(w http.ResponseWriter, r *http.Request) {
	for i := range s.cfg.Projects {
		if len(s.cfg.EnvIDs(&s.cfg.Projects[i])) > 0 {
			http.Redirect(w, r, projectURL(s.cfg.Projects[i].ID, "", ""), http.StatusFound)
			return
		}
	}
	http.Error(w, "no projects with endpoints configured", http.StatusNotFound)
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.cfg.Project(r.PathValue("project"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	envs := s.cfg.EnvIDs(p)
	if len(envs) == 0 {
		http.Error(w, "project has no endpoints configured", http.StatusNotFound)
		return
	}
	env := r.URL.Query().Get("env")
	if _, deployed := p.Endpoints[env]; !deployed {
		env = envs[0]
	}
	s.render(w, r, "project", p.Name, "containers", s.projectPage(p, env, r.URL.Query().Get("filter")))
}

func (s *Server) lookupService(r *http.Request) (*config.Project, string, inventory.Service, error) {
	p, ok := s.cfg.Project(r.PathValue("project"))
	if !ok {
		return nil, "", inventory.Service{}, errors.New("unknown project")
	}
	env := r.PathValue("env")
	if _, ok := p.Endpoints[env]; !ok {
		return nil, "", inventory.Service{}, errors.New("project is not deployed in that environment")
	}
	snap, _ := s.inv.Snapshot(inventory.Key{Project: p.ID, Env: env})
	for _, svc := range snap.Services {
		if svc.Name == r.PathValue("service") {
			return p, env, svc, nil
		}
	}
	return nil, "", inventory.Service{}, errors.New("service not found")
}

func (s *Server) handleDrawer(w http.ResponseWriter, r *http.Request) {
	p, env, svc, err := s.lookupService(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab != "logs" {
		tab = "env"
	}
	d := s.drawer(p, env, svc, tab)
	if tab == "logs" {
		d.Lines, d.LogErr = s.recentLogs(r.Context(), p, env, svc.Name)
	}
	s.renderPartial(w, "drawer", d)
}

// ---- Logs ----

var levelRe = regexp.MustCompile(`(?i)(?:^|[\s\[|"=:])(fatal|panic|error|err|warning|warn|info|debug|trace)(?:$|[\s\]|":,])`)

func detectLevel(msg string) string {
	m := levelRe.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	switch strings.ToLower(m[1]) {
	case "fatal", "panic", "error", "err":
		return "ERROR"
	case "warning", "warn":
		return "WARN"
	case "info":
		return "INFO"
	}
	return "DEBUG"
}

func toLine(l dockerapi.LogLine, tasks map[string]string) LogLine {
	t := ""
	if !l.Time.IsZero() {
		t = l.Time.Local().Format("15:04:05")
	}
	task := tasks[l.TaskID]
	if task == "" && len(l.TaskID) >= 8 {
		task = l.TaskID[:8]
	}
	msg := logmask.Mask(l.Msg)
	return LogLine{Time: t, Task: task, Level: detectLevel(msg), Msg: msg}
}

func (s *Server) recentLogs(ctx context.Context, p *config.Project, env, service string) ([]LogLine, string) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Server.RequestTimeout)
	defer cancel()
	rc, tasks, err := s.inv.Logs(ctx, inventory.Key{Project: p.ID, Env: env}, service, s.cfg.Server.LogTail, false)
	if err != nil {
		return nil, err.Error()
	}
	defer rc.Close()
	var lines []LogLine
	err = dockerapi.ReadLogs(rc, func(l dockerapi.LogLine) bool {
		lines = append(lines, toLine(l, tasks))
		return len(lines) < 5000
	})
	if err != nil && len(lines) == 0 {
		return nil, err.Error()
	}
	return lines, ""
}

// handleLogStream follows a service's logs as server-sent events. Only new
// lines are sent (tail=0); the drawer already shows the recent ones.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	p, env, svc, err := s.lookupService(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "too many log streams open, try again shortly", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	rc, tasks, err := s.inv.Logs(ctx, inventory.Key{Project: p.ID, Env: env}, svc.Name, 0, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	lines := make(chan LogLine, 256)
	go func() {
		defer close(lines)
		_ = dockerapi.ReadLogs(rc, func(l dockerapi.LogLine) bool {
			select {
			case lines <- toLine(l, tasks):
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case l, ok := <-lines:
			if !ok {
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			b, _ := json.Marshal(l)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}
