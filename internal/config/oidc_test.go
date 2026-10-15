package config

import (
	"strings"
	"testing"
)

const oidcManifest = `
version: 1
oidc:
  audience: https://deploy.img.ntest.top
services:
  main:
    dir: /srv/main
    file: compose.yml
    service: app
    image: ghcr.io/kairee/img-webdn
    oidc:
      - repository_id: 1001
        repository_owner_id: "2002"
        repository: kairee/img-webdn
        ref: refs/heads/main
        event_name: [push]
        environment: preview-main
        job_workflow_ref: kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main
`

func mainClaims() OIDCClaims {
	return OIDCClaims{
		RepositoryID:      "1001",
		RepositoryOwnerID: "2002",
		Repository:        "kairee/img-webdn",
		Ref:               "refs/heads/main",
		EventName:         "push",
		Environment:       "preview-main",
		JobWorkflowRef:    "kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main",
	}
}

func TestParse_OIDCRules(t *testing.T) {
	m, err := Parse([]byte(oidcManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.OIDCAudience() != "https://deploy.img.ntest.top" {
		t.Fatalf("audience: got %q", m.OIDCAudience())
	}
	if got := m.Services["main"].OIDC[0].RepositoryID; got != "1001" {
		t.Fatalf("unquoted numeric repository_id: got %q", got)
	}
	err = m.OIDCAllowed("main", mainClaims())
	if err != nil {
		t.Fatalf("matching claims denied: %v", err)
	}
	if len(m.AccessWarnings()) != 1 {
		t.Fatalf("only the missing access section should warn, got %v", m.AccessWarnings())
	}
}

func TestManifest_OIDCAllowedChecksEachClaim(t *testing.T) {
	m, err := Parse([]byte(oidcManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := map[string]func(c *OIDCClaims){
		"repository_id differs, same name": func(c *OIDCClaims) { c.RepositoryID = "9999" },
		"repository_owner_id differs":      func(c *OIDCClaims) { c.RepositoryOwnerID = "9999" },
		"repository name differs":          func(c *OIDCClaims) { c.Repository = "kairee/other" },
		"ref differs":                      func(c *OIDCClaims) { c.Ref = "refs/heads/dev" },
		"event not listed":                 func(c *OIDCClaims) { c.EventName = "workflow_dispatch" },
		"environment differs":              func(c *OIDCClaims) { c.Environment = "production" },
		"environment missing":              func(c *OIDCClaims) { c.Environment = "" },
		"job_workflow_ref differs": func(c *OIDCClaims) {
			c.JobWorkflowRef = "kairee/img-webdn/.github/workflows/other.yml@refs/heads/main"
		},
		"pull_request_target": func(c *OIDCClaims) { c.EventName = "pull_request_target" },
		"workflow_run":        func(c *OIDCClaims) { c.EventName = "workflow_run" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := mainClaims()
			mutate(&c)
			err := m.OIDCAllowed("main", c)
			if err == nil {
				t.Fatal("want denial")
			}
		})
	}
	err = m.OIDCAllowed("missing", mainClaims())
	if err == nil {
		t.Fatal("service without oidc rules must deny")
	}
}

func TestManifest_DeniedEventsRefusedEvenIfConfigured(t *testing.T) {
	m, err := Parse([]byte(oidcManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	svc := m.Services["main"]
	svc.OIDC[0].EventName = []OIDCEvent{OIDCEventPullRequestTarget, OIDCEventWorkflowRun}
	m.Services["main"] = svc
	for _, event := range []string{"pull_request_target", "workflow_run"} {
		c := mainClaims()
		c.EventName = event
		err = m.OIDCAllowed("main", c)
		if err == nil {
			t.Fatalf("%s must be refused at runtime even when configured", event)
		}
	}
}

func TestParse_OIDCValidation(t *testing.T) {
	cases := map[string]struct {
		old, new, wantErr string
	}{
		"pull_request_target": {"event_name: [push]", "event_name: [push, pull_request_target]", "not allowed"},
		"workflow_run":        {"event_name: [push]", "event_name: [workflow_run]", "not allowed"},
		"empty events":        {"event_name: [push]", "event_name: []", "at least one event"},
		"missing repo id":     {"- repository_id: 1001", "- repository_id: \"\"", "repository_id"},
		"non-numeric repo id": {"- repository_id: 1001", "- repository_id: kairee/img-webdn", "repository_id"},
		"missing owner id":    {`repository_owner_id: "2002"`, `repository_owner_id: ""`, "repository_owner_id"},
		"bad ref":             {"ref: refs/heads/main", "ref: main", "ref is required"},
		"github default aud": {"audience: https://deploy.img.ntest.top", "audience: https://github.com/kairee",
			"agent-specific"},
		"rules without aud": {"audience: https://deploy.img.ntest.top", `audience: ""`, "oidc.audience is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := strings.Replace(oidcManifest, tc.old, tc.new, 1)
			if data == oidcManifest {
				t.Fatalf("fixture replacement %q not found", tc.old)
			}
			_, err := Parse([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestManifest_OIDCWarnsOnBroadRule(t *testing.T) {
	data := strings.Replace(oidcManifest, "        environment: preview-main\n", "", 1)
	data = strings.Replace(data,
		"        job_workflow_ref: kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main\n", "", 1)
	m, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	warnings := m.AccessWarnings()
	if len(warnings) != 2 || !strings.Contains(warnings[1], "services.main.oidc[0]") {
		t.Fatalf("want a broad-rule warning, got %v", warnings)
	}
}
