package internal_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"container-ops/internal/auth"
	"container-ops/internal/config"
	"container-ops/internal/dockerapi"
	"container-ops/internal/envcheck"
	"container-ops/internal/harbor"
	"container-ops/internal/logmask"
)

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("HARBOR_PASSWORD", "x")
	t.Setenv("SECRET_KEY", "x")
	c, err := config.Load("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Environments) != 3 || c.Environments[0].Label != "Develop" {
		t.Fatalf("environments: %+v", c.Environments)
	}
	p, _ := c.Project("bss")
	if got := p.RequiredEnvPath("transfer"); got != "services/transfer/.env.example" {
		t.Fatalf("per-service path: %s", got)
	}
	if got := p.RequiredEnvPath("gateway"); got != ".env.example" {
		t.Fatalf("default path: %s", got)
	}
	tf, _ := c.Project("tftd")
	if ids := c.EnvIDs(tf); strings.Join(ids, ",") != "DEV,INT" {
		t.Fatalf("env ids: %v", ids)
	}
}

func TestEnvCheck(t *testing.T) {
	names := envcheck.ParseExample("# comment\nexport DB_HOST=db\nDB_PASSWORD=\n\nSETTLEMENT_QUEUE=x\nDB_HOST=dup\nbad-name=1\n")
	if strings.Join(names, ",") != "DB_HOST,DB_PASSWORD,SETTLEMENT_QUEUE" {
		t.Fatalf("names: %v", names)
	}
	present := envcheck.Presence([]string{"DB_HOST=db", "DB_PASSWORD=  ", "EXTRA=1"})
	r := envcheck.Check(names, present, true)
	if r.Missing != 1 || r.Empty != 1 || r.Required != 3 {
		t.Fatalf("result: %+v", r)
	}
	if r.Vars[0].Name != "SETTLEMENT_QUEUE" || r.Vars[0].Status != envcheck.Missing || r.Vars[1].Status != envcheck.Empty {
		t.Fatalf("order: %+v", r.Vars)
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"connecting password=hunter2 now":           "connecting password=****** now",
		`DB_PASSWORD: "s3cr et"`:                    `DB_PASSWORD: ******`,
		"url postgres://svc:pa55@db:5432/bss":       "url postgres://svc:******@db:5432/bss",
		"Authorization: Bearer abcdefghijklmnop123": "Authorization: Bearer ******",
		"api_key=abc123 user=bob":                   "api_key=****** user=bob",
		"nothing secret here":                       "nothing secret here",
	}
	for in, want := range cases {
		if got := logmask.Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestReadLogsMultiplexed(t *testing.T) {
	var b bytes.Buffer
	b.Write(frame(1, "2026-11-12T07:50:01.000000000Z com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t1 hello\n2026-11-12T07:50:02Z com.docker.swarm.node.id=n1,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t1 par"))
	b.Write(frame(1, "tial line\n"))
	b.Write(frame(2, "2026-11-12T07:50:03Z com.docker.swarm.node.id=n2,com.docker.swarm.service.id=s1,com.docker.swarm.task.id=t2 ERROR boom\n"))
	var got []dockerapi.LogLine
	if err := dockerapi.ReadLogs(&b, func(l dockerapi.LogLine) bool { got = append(got, l); return true }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Msg != "hello" || got[1].Msg != "partial line" || got[2].Stream != "stderr" || got[2].TaskID != "t2" {
		t.Fatalf("lines: %+v", got)
	}
}

func TestReadLogsRaw(t *testing.T) {
	var got []string
	_ = dockerapi.ReadLogs(strings.NewReader("2026-11-12T07:50:01Z plain tty line\nsecond\n"), func(l dockerapi.LogLine) bool {
		got = append(got, l.Msg)
		return true
	})
	if strings.Join(got, "|") != "plain tty line|second" {
		t.Fatalf("raw: %v", got)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.VerifyPassword(h, "correct horse battery") || auth.VerifyPassword(h, "wrong") {
		t.Fatal("verify mismatch")
	}
	if auth.VerifyPassword("garbage", "x") {
		t.Fatal("garbage hash accepted")
	}
}

func TestParseImage(t *testing.T) {
	r := harbor.ParseImage("harbor.local/bss/transfer:1.9.0@sha256:abc")
	if r.Registry != "harbor.local" || r.Project != "bss" || r.Repository != "transfer" || r.Tag != "1.9.0" || r.Digest != "sha256:abc" {
		t.Fatalf("%+v", r)
	}
	r = harbor.ParseImage("harbor.local:8443/team/sub/app:2@sha256:def")
	if r.Registry != "harbor.local:8443" || r.Project != "team" || r.Repository != "sub/app" || r.Tag != "2" {
		t.Fatalf("%+v", r)
	}
}
