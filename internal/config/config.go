// Package config loads and validates services.yaml, the deployment
// manifest. The manifest is
// both the mapping from logical service name to a compose directory/service
// and the deploy allowlist: services not present in it are always rejected.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// DefaultTagEnv is used when a service entry omits tag_env.
const DefaultTagEnv = "IMAGE_TAG"

var tagEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Service is a single entry under services.yaml's `services` map.
type Service struct {
	// Dir is the absolute path of the compose project directory.
	Dir string `yaml:"dir"`
	// File is the compose file name, relative to Dir.
	File string `yaml:"file"`
	// ComposeService is the service name inside the compose file that
	// this manifest entry manages. Only this service is pulled, started
	// and readiness-checked by the agent.
	ComposeService string `yaml:"service"`
	// Image is the fully qualified image repository (no tag).
	Image string `yaml:"image"`
	// TagEnv is the environment variable name used to inject the tag,
	// both as a process env var during pull/config precheck and as a
	// line written into Dir/.env before `up`. Defaults to IMAGE_TAG.
	TagEnv string `yaml:"tag_env"`
	// ImageRef selects how the deployed version is referenced: "tag"
	// (default, compose uses image:${TAG_ENV}) or "digest" (compose uses
	// image@${TAG_ENV}, and only sha256 digests are accepted).
	ImageRef ImageRefMode `yaml:"image_ref,omitempty"`
	// PostDeploy, if set, is a fixed command run inside the container after
	// the new version passes readiness. A failure rolls the deploy back.
	PostDeploy *PostDeploy `yaml:"post_deploy,omitempty"`
	// OIDC lists the GitHub Actions OIDC bindings allowed to act on this
	// service through the OIDC listener. Empty: OIDC tokens are refused.
	OIDC []OIDCRule `yaml:"oidc,omitempty"`
}

// ComposeFile returns the absolute path to the compose file.
func (s Service) ComposeFile() string {
	return filepath.Join(s.Dir, s.File)
}

// EnvFile returns the absolute path to the .env file that the agent
// rewrites before `up`.
func (s Service) EnvFile() string {
	return filepath.Join(s.Dir, ".env")
}

// EffectiveTagEnv returns TagEnv, defaulting to DefaultTagEnv when empty.
func (s Service) EffectiveTagEnv() string {
	if s.TagEnv == "" {
		return DefaultTagEnv
	}
	return s.TagEnv
}

// Manifest is the parsed and validated contents of services.yaml.
type Manifest struct {
	Version  int                `yaml:"version"`
	Services map[string]Service `yaml:"services"`
	// Access optionally restricts which client certificates may act on
	// which services. Nil keeps the historical behavior: any registered,
	// non-revoked certificate may deploy any service.
	Access *Access `yaml:"access,omitempty"`
	// OIDC configures GitHub Actions OIDC authentication (see oidc.go).
	OIDC *OIDCSettings `yaml:"oidc,omitempty"`
}

// Lookup returns the service entry for name, and whether it exists. This is
// the allowlist check: any service not present here must be rejected.
func (m *Manifest) Lookup(name string) (Service, bool) {
	svc, ok := m.Services[name]
	return svc, ok
}

// Load reads and validates a manifest file from path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return Parse(data)
}

// Parse validates YAML bytes into a Manifest, applying defaults.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version %d, expected 1", m.Version)
	}
	if len(m.Services) == 0 {
		return nil, fmt.Errorf("manifest has no services")
	}

	// dir -> tag_env -> service name, used to detect collisions between
	// entries sharing the same compose directory.
	dirTagEnv := map[string]map[string]string{}
	dirComposeService := map[string]map[string]string{}

	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := m.Services[name]
		svc = applyServiceDefaults(svc)
		m.Services[name] = svc
		err := validateService(name, svc)
		if err != nil {
			return nil, err
		}

		if dirTagEnv[svc.Dir] == nil {
			dirTagEnv[svc.Dir] = map[string]string{}
			dirComposeService[svc.Dir] = map[string]string{}
		}
		if existing, ok := dirTagEnv[svc.Dir][svc.TagEnv]; ok && existing != name {
			return nil, fmt.Errorf("services %q and %q share dir %q with the same tag_env %q; tag_env must be unique per directory", existing, name, svc.Dir, svc.TagEnv)
		}
		dirTagEnv[svc.Dir][svc.TagEnv] = name
		if existing, ok := dirComposeService[svc.Dir][svc.ComposeService]; ok && existing != name {
			return nil, fmt.Errorf("services %q and %q share dir %q with the same compose service %q; compose service must be unique per directory", existing, name, svc.Dir, svc.ComposeService)
		}
		dirComposeService[svc.Dir][svc.ComposeService] = name
	}

	err := m.validateAccess()
	if err != nil {
		return nil, err
	}
	err = m.validateOIDC()
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// applyServiceDefaults fills in the optional fields Parse defaults.
func applyServiceDefaults(svc Service) Service {
	if svc.TagEnv == "" {
		svc.TagEnv = DefaultTagEnv
	}
	if svc.PostDeploy != nil {
		pd := *svc.PostDeploy
		if pd.Service == "" {
			pd.Service = svc.ComposeService
		}
		if pd.Timeout == 0 {
			pd.Timeout = DefaultPostDeployTimeout
		}
		svc.PostDeploy = &pd
	}
	return svc
}

func validateService(name string, svc Service) error {
	if svc.Dir == "" {
		return fmt.Errorf("service %q: dir is required", name)
	}
	if !filepath.IsAbs(svc.Dir) {
		return fmt.Errorf("service %q: dir must be an absolute path, got %q", name, svc.Dir)
	}
	if svc.File == "" {
		return fmt.Errorf("service %q: file is required", name)
	}
	if svc.ComposeService == "" {
		return fmt.Errorf("service %q: service (compose service name) is required", name)
	}
	if svc.Image == "" {
		return fmt.Errorf("service %q: image is required", name)
	}
	if !tagEnvPattern.MatchString(svc.TagEnv) {
		return fmt.Errorf("service %q: tag_env %q is not a valid environment variable name", name, svc.TagEnv)
	}
	switch svc.ImageRef {
	case "", ImageRefTag, ImageRefDigest:
	default:
		return fmt.Errorf("service %q: image_ref %q must be %q or %q", name, svc.ImageRef, ImageRefTag, ImageRefDigest)
	}
	if svc.PostDeploy != nil {
		err := validatePostDeploy(*svc.PostDeploy)
		if err != nil {
			return fmt.Errorf("service %q: post_deploy: %w", name, err)
		}
	}
	return nil
}

// ValidatePaths checks that Dir and Dir/File exist on disk. Kept separate
// from Parse so unit tests can validate manifest structure without needing
// real filesystem fixtures for every case.
func (m *Manifest) ValidatePaths() error {
	for name, svc := range m.Services {
		info, err := os.Stat(svc.Dir)
		if err != nil {
			return fmt.Errorf("service %q: dir %q: %w", name, svc.Dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("service %q: dir %q is not a directory", name, svc.Dir)
		}
		composeFile := svc.ComposeFile()
		if _, err := os.Stat(composeFile); err != nil {
			return fmt.Errorf("service %q: compose file %q: %w", name, composeFile, err)
		}
	}
	return nil
}
