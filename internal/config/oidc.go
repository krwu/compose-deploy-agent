package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// OIDCEvent is a GitHub Actions event_name claim value.
type OIDCEvent string

const (
	// OIDCEventPush is the push event.
	OIDCEventPush OIDCEvent = "push"
	// OIDCEventPullRequestTarget runs with the base repository's
	// privileges on code controlled by the PR author; always refused.
	OIDCEventPullRequestTarget OIDCEvent = "pull_request_target"
	// OIDCEventWorkflowRun runs privileged follow-up workflows that are
	// commonly fed by untrusted artifacts; always refused.
	OIDCEventWorkflowRun OIDCEvent = "workflow_run"
)

// Denied reports whether e is refused regardless of configuration.
func (e OIDCEvent) Denied() bool {
	return e == OIDCEventPullRequestTarget || e == OIDCEventWorkflowRun
}

var (
	numericIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	eventNamePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

// OIDCSettings is the top-level `oidc` section of services.yaml.
type OIDCSettings struct {
	// Audience is the agent-specific aud value every token must carry,
	// e.g. "https://deploy.img.ntest.top". Empty disables OIDC.
	Audience string `yaml:"audience"`
}

// OIDCRule is one entry of a service's `oidc` list. A token is allowed
// for the service when every configured field of at least one rule
// equals the matching claim. Each claim is compared on its own; sub is
// never used.
type OIDCRule struct {
	// RepositoryID and RepositoryOwnerID bind the numeric ids, which
	// survive renames and cannot be re-registered by someone else.
	RepositoryID      string `yaml:"repository_id"`
	RepositoryOwnerID string `yaml:"repository_owner_id"`
	// Repository ("owner/name") is optional and only a second check.
	Repository string `yaml:"repository,omitempty"`
	// Ref is the exact git ref, e.g. refs/heads/main.
	Ref string `yaml:"ref"`
	// EventName is the allowlist of event_name values.
	EventName []OIDCEvent `yaml:"event_name"`
	// Environment, if set, must equal the environment claim.
	Environment string `yaml:"environment,omitempty"`
	// JobWorkflowRef, if set, must equal the job_workflow_ref claim, e.g.
	// kairee/img-webdn/.github/workflows/deploy.yml@refs/heads/main.
	JobWorkflowRef string `yaml:"job_workflow_ref,omitempty"`
}

// OIDCClaims is the subset of verified token claims the rules check.
type OIDCClaims struct {
	RepositoryID      string
	RepositoryOwnerID string
	Repository        string
	Ref               string
	EventName         string
	Environment       string
	JobWorkflowRef    string
}

// OIDCAudience returns the configured audience, or "" when OIDC is off.
func (m *Manifest) OIDCAudience() string {
	if m.OIDC == nil {
		return ""
	}
	return m.OIDC.Audience
}

// OIDCAllowed reports whether verified claims may act on service. On
// denial the error explains which check failed, for the log only.
func (m *Manifest) OIDCAllowed(service string, c OIDCClaims) error {
	if OIDCEvent(c.EventName).Denied() {
		return fmt.Errorf("event_name %q is always refused", c.EventName)
	}
	svc, ok := m.Services[service]
	if !ok || len(svc.OIDC) == 0 {
		return fmt.Errorf("service %q has no oidc rules", service)
	}
	errs := make([]error, 0, len(svc.OIDC))
	for i, rule := range svc.OIDC {
		err := rule.match(c)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("rule %d: %w", i, err))
	}
	return errors.Join(errs...)
}

func (r OIDCRule) match(c OIDCClaims) error {
	switch {
	case c.RepositoryID != r.RepositoryID:
		return errors.New("repository_id mismatch")
	case c.RepositoryOwnerID != r.RepositoryOwnerID:
		return errors.New("repository_owner_id mismatch")
	case r.Repository != "" && c.Repository != r.Repository:
		return errors.New("repository mismatch")
	case c.Ref != r.Ref:
		return errors.New("ref mismatch")
	case !slices.Contains(r.EventName, OIDCEvent(c.EventName)):
		return errors.New("event_name not allowed")
	case r.Environment != "" && c.Environment != r.Environment:
		return errors.New("environment mismatch")
	case r.JobWorkflowRef != "" && c.JobWorkflowRef != r.JobWorkflowRef:
		return errors.New("job_workflow_ref mismatch")
	default:
		return nil
	}
}

func (r OIDCRule) validate() error {
	if !numericIDPattern.MatchString(r.RepositoryID) {
		return errors.New("repository_id must be the numeric repository id")
	}
	if !numericIDPattern.MatchString(r.RepositoryOwnerID) {
		return errors.New("repository_owner_id must be the numeric owner id")
	}
	if !strings.HasPrefix(r.Ref, "refs/") {
		return errors.New("ref is required and must start with refs/")
	}
	if len(r.EventName) == 0 {
		return errors.New("event_name must list at least one event")
	}
	for _, e := range r.EventName {
		if e.Denied() {
			return fmt.Errorf("event_name %q is not allowed", e)
		}
		if !eventNamePattern.MatchString(string(e)) {
			return fmt.Errorf("event_name %q is not a valid event name", e)
		}
	}
	return nil
}

// validateOIDC checks the oidc section and every service's oidc rules.
func (m *Manifest) validateOIDC() error {
	hasRules := false
	for name, svc := range m.Services {
		for i, rule := range svc.OIDC {
			err := rule.validate()
			if err != nil {
				return fmt.Errorf("service %q: oidc[%d]: %w", name, i, err)
			}
			hasRules = true
		}
	}
	aud := m.OIDCAudience()
	if hasRules && aud == "" {
		return errors.New("oidc.audience is required when a service has oidc rules")
	}
	if strings.HasPrefix(strings.ToLower(aud), "https://github.com/") {
		return errors.New("oidc.audience must be an agent-specific value, not GitHub's default https://github.com/<owner>")
	}
	return nil
}

// oidcWarnings flags rules that bind neither an environment nor a
// job_workflow_ref: any workflow of the repository on that ref and event
// could then deploy the service.
func (m *Manifest) oidcWarnings() []string {
	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	var warnings []string
	for _, name := range names {
		for i, rule := range m.Services[name].OIDC {
			if rule.Environment == "" && rule.JobWorkflowRef == "" {
				warnings = append(warnings, fmt.Sprintf("services.%s.oidc[%d] sets neither environment nor "+
					"job_workflow_ref: any workflow of the repository on %s may deploy it", name, i, rule.Ref))
			}
		}
	}
	return warnings
}
