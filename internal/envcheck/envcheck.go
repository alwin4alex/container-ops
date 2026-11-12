// Package envcheck compares the environment variables a service actually has
// with the list it requires. Only names and "set / empty" are kept; values
// are dropped as soon as they are read and never leave this package.
package envcheck

import (
	"bufio"
	"sort"
	"strings"
)

type Status string

const (
	OK      Status = "ok"
	Missing Status = "missing"
	Empty   Status = "empty"
	Extra   Status = "extra" // set on the service but not in the required list
)

type Var struct {
	Name   string
	Status Status
}

type Result struct {
	Vars     []Var // missing first, then empty, then ok (alphabetical within)
	Missing  int
	Empty    int
	Required int
	// Known is false when no required list could be loaded; the check is
	// then reported as unknown instead of silently passing.
	Known bool
}

func (r Result) Issues() int { return r.Missing + r.Empty }

// ParseExample reads a .env.example style file and returns the variable
// names it declares. Comments, blank lines and "export " prefixes are
// handled; values in the example file are ignored.
func ParseExample(content string) []string {
	seen := map[string]bool{}
	var names []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, _, _ := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !validName(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func validName(n string) bool {
	if n == "" {
		return false
	}
	for i, r := range n {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// Presence turns a container's "KEY=VALUE" list into name -> hasValue,
// discarding the values.
func Presence(env []string) map[string]bool {
	p := make(map[string]bool, len(env))
	for _, kv := range env {
		k, v, found := strings.Cut(kv, "=")
		p[k] = found && strings.TrimSpace(v) != ""
	}
	return p
}

// Check compares required names with what the service has.
func Check(required []string, present map[string]bool, known bool) Result {
	r := Result{Known: known, Required: len(required)}
	for _, name := range required {
		has, ok := present[name]
		switch {
		case !ok:
			r.Vars = append(r.Vars, Var{name, Missing})
			r.Missing++
		case !has:
			r.Vars = append(r.Vars, Var{name, Empty})
			r.Empty++
		default:
			r.Vars = append(r.Vars, Var{name, OK})
		}
	}
	rank := map[Status]int{Missing: 0, Empty: 1, OK: 2}
	sort.SliceStable(r.Vars, func(i, j int) bool {
		if rank[r.Vars[i].Status] != rank[r.Vars[j].Status] {
			return rank[r.Vars[i].Status] < rank[r.Vars[j].Status]
		}
		return r.Vars[i].Name < r.Vars[j].Name
	})
	return r
}
