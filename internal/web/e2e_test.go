package web_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"container-ops/internal/auth"
	"container-ops/internal/config"
	"container-ops/internal/github"
	"container-ops/internal/harbor"
	"container-ops/internal/inventory"
	"container-ops/internal/web"
)

func frame(s string) []byte {
	h := make([]byte, 8)
	h[0] = 1
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

// fakeDocker serves just enough of the Engine API for one stack "bss".
func fakeDocker(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"ServerVersion": "28.3.3", "NCPU": 8, "MemTotal": int64(16) << 30, "Images": 42,
			"Swarm": map[string]any{"LocalNodeState": "active", "ControlAvailable": true, "Nodes": 3}})
	})
	mux.HandleFunc("/services", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), "com.docker.stack.namespace=bss") {
			t.Errorf("services filter: %s", r.URL.RawQuery)
		}
		one, two := uint64(1), uint64(2)
		svc := func(id, name, image string, replicas *uint64, env []string) map[string]any {
			return map[string]any{"ID": id, "UpdatedAt": "2026-11-12T07:52:00Z", "Spec": map[string]any{
				"Name": name, "Labels": map[string]string{"com.docker.stack.namespace": "bss", "deploy.run": "418"},
				"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": image, "Env": env}},
				"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": replicas}},
			}}
		}
		j(w, []any{
			svc("s-transfer", "bss_transfer", "harbor.local/bss/transfer:1.9.0@sha256:e51c0b7f2a91aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", &two,
				[]string{"DB_HOST=db", "DB_PASSWORD=supersecretvalue", "SETTLEMENT_QUEUE="}),
			svc("s-report", "bss_report", "harbor.local/bss/report:1.6.1@sha256:7ab35e60", &one,
				[]string{"DB_HOST=db", "DB_PASSWORD=supersecretvalue", "SETTLEMENT_QUEUE=q", "CORE_BANK_API_URL=http://core"}),
		})
	})
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		task := func(id, svc, node string, slot int, state string) map[string]any {
			return map[string]any{"ID": id, "ServiceID": svc, "NodeID": node, "Slot": slot, "DesiredState": "running",
				"Status": map[string]any{"State": state}}
		}
		j(w, []any{
			task("t1", "s-transfer", "n1", 1, "running"),
			task("t2", "s-transfer", "n2", 2, "running"),
			task("t3", "s-report", "n1", 1, "starting"),
		})
	})
	mux.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
		j(w, []any{
			map[string]any{"ID": "n1", "Description": map[string]any{"Hostname": "node-01"}, "Status": map[string]any{"State": "ready"}},
			map[string]any{"ID": "n2", "Description": map[string]any{"Hostname": "node-02"}, "Status": map[string]any{"State": "ready"}},
		})
	})
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"Volumes": []any{map[string]any{"Name": "a"}, map[string]any{"Name": "b"}}})
	})
	mux.HandleFunc("/services/s-transfer/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Write(frame("2026-11-12T07:50:01Z com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s-transfer,com.docker.swarm.task.id=t1 ERROR Required environment variable CORE_BANK_API_URL is not set\n"))
		w.Write(frame("2026-11-12T07:50:02Z com.docker.swarm.node.id=n2,com.docker.swarm.service.id=s-transfer,com.docker.swarm.task.id=t2 INFO connecting password=hunter2 <script>x</script>\n"))
	})
	return httptest.NewServer(mux)
}

type dispatch struct {
	Ref    string            `json:"ref"`
	Inputs map[string]string `json:"inputs"`
}

func fakeGitHub(t *testing.T, got *[]dispatch, mu *sync.Mutex) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/bss-deploy/contents/.env.example", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-test" {
			t.Errorf("missing token")
		}
		io.WriteString(w, "DB_HOST=\nDB_PASSWORD=\nSETTLEMENT_QUEUE=\nCORE_BANK_API_URL=\n")
	})
	mux.HandleFunc("POST /repos/org/bss-deploy/actions/workflows/deploy-stack.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		var d dispatch
		json.NewDecoder(r.Body).Decode(&d)
		mu.Lock()
		*got = append(*got, d)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/org/bss-deploy/actions/workflows/deploy-stack.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"workflow_runs":[{"id":1,"run_number":418,"display_title":"deploy bss → DEV","status":"completed","conclusion":"success","html_url":"https://github.com/org/bss-deploy/actions/runs/1","head_branch":"main","created_at":"2026-11-12T07:52:00Z","actor":{"login":"alexa"}}]}`)
	})
	return httptest.NewServer(mux)
}

type env struct {
	srv        *httptest.Server
	dispatches *[]dispatch
	mu         *sync.Mutex
}

func setup(t *testing.T) env {
	t.Helper()
	docker := fakeDocker(t)
	t.Cleanup(docker.Close)
	var got []dispatch
	mu := &sync.Mutex{}
	gh := fakeGitHub(t, &got, mu)
	t.Cleanup(gh.Close)
	deployerHash, _ := auth.HashPassword("deployer-password")
	viewerHash, _ := auth.HashPassword("viewer-password")
	cfgYAML := `
server: { refresh_interval: 1h, request_timeout: 2s }
auth:
  mode: basic
  users:
    - { name: alexa, role: deployer, password_hash: "` + deployerHash + `" }
    - { name: guest, role: viewer, password_hash: "` + viewerHash + `" }
github: { api_url: "` + gh.URL + `" }
environments:
  - { id: DEV, label: Develop }
  - { id: INT, label: Integration }
  - { id: SIT, label: SIT, require_confirm: true, require_approval: true }
projects:
  - id: bss
    name: BSS
    repo: org/bss-deploy
    endpoints:
      DEV: { docker: "` + docker.URL + `", swarm: swarm-dev }
      INT: { docker: "http://127.0.0.1:1", swarm: swarm-int }
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(cfgYAML), 0o600)
	t.Setenv("GITHUB_TOKEN", "gh-test")
	t.Setenv("SECRET_KEY", "test-secret")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ghc := github.New(cfg.GitHub.APIURL, cfg.GitHub.Token, 2*time.Second)
	hb, _ := harbor.New("", "", "", "", time.Second)
	inv, err := inventory.New(cfg, ghc, hb)
	if err != nil {
		t.Fatal(err)
	}
	inv.RefreshAll(context.Background())
	a, _ := auth.New(cfg.Auth, cfg.Server.SecretKey)
	s, err := web.New(cfg, inv, ghc, a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return env{srv: srv, dispatches: &got, mu: mu}
}

func (e env) req(t *testing.T, method, path, user, pass string, form url.Values, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, body)
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func mustContain(t *testing.T, body string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Errorf("response does not contain %q", p)
		}
	}
}

var csrfRe = regexp.MustCompile(`X-CSRF-Token&#34;: &#34;([0-9a-f]+)&#34;|"X-CSRF-Token": "([0-9a-f]+)"`)

func csrf(t *testing.T, body string) string {
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token in page")
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

func TestDashboardEndToEnd(t *testing.T) {
	e := setup(t)

	// Auth required
	if code, _, _ := e.req(t, "GET", "/", "", "", nil, nil); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
	if code, _, _ := e.req(t, "GET", "/", "alexa", "wrong", nil, nil); code != 401 {
		t.Fatalf("bad password: %d", code)
	}

	// Projects page
	code, body, hdr := e.req(t, "GET", "/", "alexa", "deployer-password", nil, nil)
	if code != 200 {
		t.Fatalf("projects: %d %s", code, body)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Error("missing CSP")
	}
	mustContain(t, body, "BSS", "Partly down", "Develop · Up", "Integration · Down", "SIT · Not deployed", "2 services", "3 containers", "8 CPU")

	// Containers page, DEV
	_, body, _ = e.req(t, "GET", "/projects/bss?env=DEV", "alexa", "deployer-password", nil, nil)
	mustContain(t, body, "transfer", "report", "1 missing, 1 empty", "0/1 running", "2/2 running", "4/4 OK", "bss/transfer:1.9.0", "sha256:e51c0b7f2a91", "swarm-dev")
	if strings.Contains(body, "supersecretvalue") {
		t.Fatal("env value leaked into page")
	}

	// Down endpoint shows a banner
	_, body, _ = e.req(t, "GET", "/projects/bss?env=INT", "alexa", "deployer-password", nil, nil)
	mustContain(t, body, "cannot be reached")

	// Env filter
	_, body, _ = e.req(t, "GET", "/projects/bss?env=DEV&filter=env", "alexa", "deployer-password", nil, nil)
	if strings.Contains(body, ">report<") {
		t.Error("env filter should hide report")
	}

	// Drawer: env vars
	_, body, _ = e.req(t, "GET", "/projects/bss/envs/DEV/services/transfer?tab=env", "alexa", "deployer-password", nil, nil)
	mustContain(t, body, "CORE_BANK_API_URL", "Missing", "SETTLEMENT_QUEUE", "Empty", "1 missing, 1 empty of 4")
	if strings.Contains(body, "supersecretvalue") {
		t.Fatal("env value leaked into drawer")
	}

	// Drawer: logs are masked and escaped, tasks resolved to slot@node
	_, body, _ = e.req(t, "GET", "/projects/bss/envs/DEV/services/transfer?tab=logs", "alexa", "deployer-password", nil, nil)
	mustContain(t, body, "password=******", ".2@node-02", "ERROR", "&lt;script&gt;")
	if strings.Contains(body, "hunter2") || strings.Contains(body, "<script>x") {
		t.Fatal("secret or raw html leaked in logs")
	}

	// Log stream (SSE) ends cleanly when the docker stream ends
	_, body, hdr = e.req(t, "GET", "/projects/bss/envs/DEV/services/transfer/logs/stream", "alexa", "deployer-password", nil, nil)
	if hdr.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream content type %q", hdr.Get("Content-Type"))
	}
	mustContain(t, body, `"Level":"ERROR"`, "event: end")

	// Deploy: CSRF required
	form := url.Values{"project": {"bss"}, "env": {"DEV"}, "action": {"deploy"}, "ref": {"main"}}
	_, body, _ = e.req(t, "POST", "/deploy/confirm", "alexa", "deployer-password", form, nil)
	mustContain(t, body, "security token")

	_, page, _ := e.req(t, "GET", "/deploy?project=bss", "alexa", "deployer-password", nil, nil)
	token := csrf(t, page)
	h := map[string]string{"X-CSRF-Token": token}

	_, body, _ = e.req(t, "POST", "/deploy/confirm", "alexa", "deployer-password", form, h)
	mustContain(t, body, "Deploy BSS to Develop?")
	if strings.Contains(body, "confirm_text") {
		t.Error("DEV deploy should not need typed confirmation")
	}
	_, body, hdr = e.req(t, "POST", "/deploy/run", "alexa", "deployer-password", form, h)
	mustContain(t, body, "queued in GitHub Actions")
	if hdr.Get("HX-Trigger") != "refreshRuns" {
		t.Error("no runs refresh trigger")
	}

	// SIT needs typed confirmation and is flagged as a first deploy
	sit := url.Values{"project": {"bss"}, "env": {"SIT"}, "action": {"deploy"}, "ref": {"main"}}
	_, body, _ = e.req(t, "POST", "/deploy/confirm", "alexa", "deployer-password", sit, h)
	mustContain(t, body, "confirm_text", "approve in GitHub", "First deploy")
	_, body, _ = e.req(t, "POST", "/deploy/run", "alexa", "deployer-password", sit, h)
	mustContain(t, body, "Type bss exactly")

	// Teardown skips environments where the project is not deployed and needs typing
	td := url.Values{"project": {"bss"}, "env": {"DEV", "SIT"}, "action": {"teardown"}, "ref": {"main"}, "confirm_text": {"bss"}}
	_, body, _ = e.req(t, "POST", "/deploy/confirm", "alexa", "deployer-password", td, h)
	mustContain(t, body, "Tear down BSS in Develop?", "skipped")
	_, body, _ = e.req(t, "POST", "/deploy/run", "alexa", "deployer-password", td, h)
	mustContain(t, body, "Teardown of BSS queued")

	// Bad ref is rejected
	bad := url.Values{"project": {"bss"}, "env": {"DEV"}, "action": {"deploy"}, "ref": {"main;rm -rf"}}
	_, body, _ = e.req(t, "POST", "/deploy/run", "alexa", "deployer-password", bad, h)
	mustContain(t, body, "not a valid branch")

	// Viewer cannot deploy even with a valid token for themselves
	_, vpage, _ := e.req(t, "GET", "/deploy", "guest", "viewer-password", nil, nil)
	mustContain(t, vpage, "Your role is viewer")
	vh := map[string]string{"X-CSRF-Token": csrf(t, vpage)}
	_, body, _ = e.req(t, "POST", "/deploy/run", "guest", "viewer-password", form, vh)
	mustContain(t, body, "cannot deploy")

	// Runs list
	_, body, _ = e.req(t, "GET", "/deploy/runs?project=bss", "alexa", "deployer-password", nil, nil)
	mustContain(t, body, "deploy bss → DEV", "Succeeded")

	e.mu.Lock()
	defer e.mu.Unlock()
	d := *e.dispatches
	if len(d) != 2 {
		t.Fatalf("expected 2 dispatches (DEV deploy, DEV teardown), got %d: %+v", len(d), d)
	}
	if d[0].Ref != "main" || d[0].Inputs["action"] != "deploy" || d[0].Inputs["environment"] != "DEV" || d[0].Inputs["stack"] != "bss" || d[0].Inputs["requested_by"] != "alexa" {
		t.Fatalf("dispatch inputs: %+v", d[0])
	}
	if d[1].Inputs["action"] != "teardown" || d[1].Inputs["environment"] != "DEV" {
		t.Fatalf("teardown inputs: %+v", d[1])
	}
}
