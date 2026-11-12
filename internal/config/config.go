// Package config loads the dashboard configuration. Environments, projects and
// endpoints all live in one YAML file so nothing is hard-coded in the binary.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

type Config struct {
	Server       Server        `yaml:"server"`
	Auth         Auth          `yaml:"auth"`
	GitHub       GitHub        `yaml:"github"`
	Harbor       Harbor        `yaml:"harbor"`
	Environments []Environment `yaml:"environments"`
	Projects     []Project     `yaml:"projects"`
}

type Server struct {
	Listen          string        `yaml:"listen"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	LogTail         int           `yaml:"log_tail"`
	MaxLogStreams   int           `yaml:"max_log_streams"`
	// Secret used to sign CSRF tokens. Set it (or SECRET_KEY_FILE) so tokens
	// survive restarts and work across replicas; a random one is used otherwise.
	SecretKey     string `yaml:"secret_key"`
	SecretKeyFile string `yaml:"secret_key_file"`
}

type Auth struct {
	// Mode: "basic" (users below), "header" (trust a reverse proxy header), or
	// "none" (only for local development).
	Mode string `yaml:"mode"`
	// Header mode: the header carrying the user name, and the networks the
	// reverse proxy connects from. Requests from elsewhere are rejected.
	UserHeader     string   `yaml:"user_header"`
	RoleHeader     string   `yaml:"role_header"`
	TrustedProxies []string `yaml:"trusted_proxies"`
	Users          []User   `yaml:"users"`
	// Role given to header-mode users when no role header is sent.
	DefaultRole string `yaml:"default_role"`
}

type User struct {
	Name         string `yaml:"name"`
	PasswordHash string `yaml:"password_hash"`
	Role         string `yaml:"role"` // viewer | deployer
}

type GitHub struct {
	APIURL    string `yaml:"api_url"`
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
}

type Harbor struct {
	URL          string `yaml:"url"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	CAFile       string `yaml:"ca_file"`
}

type Environment struct {
	ID              string `yaml:"id"`
	Label           string `yaml:"label"`
	RequireConfirm  bool   `yaml:"require_confirm"`
	RequireApproval bool   `yaml:"require_approval"`
}

type Project struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Group    string `yaml:"group"`
	Stack    string `yaml:"stack"`    // Swarm stack namespace
	Repo     string `yaml:"repo"`     // owner/name holding compose, .env.example and the workflow
	Workflow string `yaml:"workflow"` // workflow file name, e.g. deploy-stack.yml
	Ref      string `yaml:"ref"`      // default branch or tag to deploy
	// Required env var lists (.env.example style). Default applies to every
	// service; Services overrides it per service name (without stack prefix).
	RequiredEnv RequiredEnv         `yaml:"required_env"`
	Endpoints   map[string]Endpoint `yaml:"endpoints"` // keyed by environment id
}

type RequiredEnv struct {
	Default  string            `yaml:"default"`
	Services map[string]string `yaml:"services"`
}

type Endpoint struct {
	// Docker API URL of a Swarm manager, normally a read-only socket proxy:
	// http://10.10.200.100:2375 or https://... with the TLS files below.
	Docker  string `yaml:"docker"`
	Swarm   string `yaml:"swarm"` // display name, e.g. swarm-dev
	TLSCA   string `yaml:"tls_ca"`
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// Load reads and validates the config file, then resolves secret files.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.UnmarshalWithOptions(raw, &c, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.RefreshInterval <= 0 {
		c.Server.RefreshInterval = 30 * time.Second
	}
	if c.Server.RequestTimeout <= 0 {
		c.Server.RequestTimeout = 10 * time.Second
	}
	if c.Server.LogTail <= 0 {
		c.Server.LogTail = 200
	}
	if c.Server.MaxLogStreams <= 0 {
		c.Server.MaxLogStreams = 20
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "basic"
	}
	if c.Auth.DefaultRole == "" {
		c.Auth.DefaultRole = "viewer"
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = "https://api.github.com"
	}
	for i := range c.Environments {
		if c.Environments[i].Label == "" {
			c.Environments[i].Label = c.Environments[i].ID
		}
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Name == "" {
			p.Name = p.ID
		}
		if p.Group == "" {
			p.Group = p.ID
		}
		if p.Stack == "" {
			p.Stack = p.ID
		}
		if p.Workflow == "" {
			p.Workflow = "deploy-stack.yml"
		}
		if p.Ref == "" {
			p.Ref = "main"
		}
		if p.RequiredEnv.Default == "" {
			p.RequiredEnv.Default = ".env.example"
		}
		for id, ep := range p.Endpoints {
			if ep.Swarm == "" {
				ep.Swarm = strings.ToLower(id)
				p.Endpoints[id] = ep
			}
		}
	}
}

// fromEnvOrFile lets secrets come from the environment (Docker secrets
// usually arrive as files) instead of the config file itself.
func fromEnvOrFile(current, file, envName string) (string, error) {
	if v := os.Getenv(envName); v != "" {
		return v, nil
	}
	if f := os.Getenv(envName + "_FILE"); f != "" {
		file = f
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return current, nil
}

func (c *Config) resolveSecrets() error {
	var err error
	if c.GitHub.Token, err = fromEnvOrFile(c.GitHub.Token, c.GitHub.TokenFile, "GITHUB_TOKEN"); err != nil {
		return err
	}
	if c.Harbor.Password, err = fromEnvOrFile(c.Harbor.Password, c.Harbor.PasswordFile, "HARBOR_PASSWORD"); err != nil {
		return err
	}
	if c.Server.SecretKey, err = fromEnvOrFile(c.Server.SecretKey, c.Server.SecretKeyFile, "SECRET_KEY"); err != nil {
		return err
	}
	return nil
}

func (c *Config) Validate() error {
	var errs []error
	if len(c.Environments) == 0 {
		errs = append(errs, errors.New("environments: at least one is required"))
	}
	envs := map[string]bool{}
	for _, e := range c.Environments {
		if !idPattern.MatchString(e.ID) {
			errs = append(errs, fmt.Errorf("environment id %q is not valid", e.ID))
		}
		if envs[e.ID] {
			errs = append(errs, fmt.Errorf("environment %q is defined twice", e.ID))
		}
		envs[e.ID] = true
	}
	projects := map[string]bool{}
	for _, p := range c.Projects {
		if !idPattern.MatchString(p.ID) {
			errs = append(errs, fmt.Errorf("project id %q is not valid", p.ID))
		}
		if projects[p.ID] {
			errs = append(errs, fmt.Errorf("project %q is defined twice", p.ID))
		}
		projects[p.ID] = true
		if !idPattern.MatchString(p.Stack) {
			errs = append(errs, fmt.Errorf("project %q: stack %q is not valid", p.ID, p.Stack))
		}
		if p.Repo != "" && strings.Count(p.Repo, "/") != 1 {
			errs = append(errs, fmt.Errorf("project %q: repo must be owner/name", p.ID))
		}
		for envID, ep := range p.Endpoints {
			if !envs[envID] {
				errs = append(errs, fmt.Errorf("project %q: endpoint for unknown environment %q", p.ID, envID))
			}
			if !strings.HasPrefix(ep.Docker, "http://") && !strings.HasPrefix(ep.Docker, "https://") {
				errs = append(errs, fmt.Errorf("project %q/%s: docker must be an http(s) URL", p.ID, envID))
			}
		}
	}
	switch c.Auth.Mode {
	case "basic":
		if len(c.Auth.Users) == 0 {
			errs = append(errs, errors.New("auth: basic mode needs at least one user"))
		}
		for _, u := range c.Auth.Users {
			if u.Role != "viewer" && u.Role != "deployer" {
				errs = append(errs, fmt.Errorf("auth user %q: role must be viewer or deployer", u.Name))
			}
		}
	case "header":
		if c.Auth.UserHeader == "" || len(c.Auth.TrustedProxies) == 0 {
			errs = append(errs, errors.New("auth: header mode needs user_header and trusted_proxies"))
		}
	case "none":
	default:
		errs = append(errs, fmt.Errorf("auth: unknown mode %q", c.Auth.Mode))
	}
	return errors.Join(errs...)
}

func (c *Config) Env(id string) (Environment, bool) {
	for _, e := range c.Environments {
		if e.ID == id {
			return e, true
		}
	}
	return Environment{}, false
}

func (c *Config) Project(id string) (*Project, bool) {
	for i := range c.Projects {
		if c.Projects[i].ID == id {
			return &c.Projects[i], true
		}
	}
	return nil, false
}

// EnvIDs returns the environments a project is deployed to, in config order.
func (c *Config) EnvIDs(p *Project) []string {
	var ids []string
	for _, e := range c.Environments {
		if _, ok := p.Endpoints[e.ID]; ok {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// RequiredEnvPath returns the .env.example path that applies to a service.
func (p *Project) RequiredEnvPath(service string) string {
	if path, ok := p.RequiredEnv.Services[service]; ok {
		return path
	}
	return p.RequiredEnv.Default
}
