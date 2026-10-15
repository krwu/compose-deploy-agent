package config

import (
	"errors"
	"fmt"
	"slices"
)

// Access binds client certificates to the services they may act on.
type Access struct {
	// RequireBinding, when true, denies every certificate that has no
	// entry in Clients. When false (the default), unlisted certificates
	// keep the historical "any service" access.
	RequireBinding bool `yaml:"require_binding,omitempty"`
	// Clients maps a certificate name (`ca issue -n`) or fingerprint (as
	// shown by `ca list`) to its binding. A fingerprint entry takes
	// precedence over a name entry for the same certificate.
	Clients map[string]ClientBinding `yaml:"clients,omitempty"`
}

// ClientBinding lists the logical services one certificate may act on.
type ClientBinding struct {
	Services []string `yaml:"services"`
}

// ClientAllowed reports whether the client certificate identified by name
// and fingerprint may deploy, or read the status of, service.
func (m *Manifest) ClientAllowed(name, fingerprint, service string) bool {
	if m.Access == nil {
		return true
	}
	binding, ok := m.Access.Clients[fingerprint]
	if !ok {
		binding, ok = m.Access.Clients[name]
	}
	if !ok {
		return !m.Access.RequireBinding
	}
	return slices.Contains(binding.Services, service)
}

// AccessWarnings describes configurations in which some certificate can
// deploy every service, or an OIDC rule is broader than recommended; serve
// logs them at startup and `serve -t` prints them.
func (m *Manifest) AccessWarnings() []string {
	return append(m.certAccessWarnings(), m.oidcWarnings()...)
}

func (m *Manifest) certAccessWarnings() []string {
	switch {
	case m.Access == nil || (len(m.Access.Clients) == 0 && !m.Access.RequireBinding):
		return []string{"access.clients is not configured: " +
			"every registered client certificate may deploy every service"}
	case !m.Access.RequireBinding:
		return []string{"access.require_binding is false: client certificates not listed in access.clients " +
			"may deploy every service"}
	case len(m.Access.Clients) == 0:
		return []string{"access.require_binding is true but access.clients is empty: " +
			"every mTLS request will be denied; only the local unix socket (and OIDC, if enabled) can deploy"}
	default:
		return nil
	}
}

func (m *Manifest) validateAccess() error {
	if m.Access == nil {
		return nil
	}
	for key, binding := range m.Access.Clients {
		if key == "" {
			return errors.New("access.clients: empty certificate name")
		}
		if len(binding.Services) == 0 {
			return fmt.Errorf("access.clients.%s: services must not be empty (revoke the certificate instead)", key)
		}
		for _, svc := range binding.Services {
			_, ok := m.Services[svc]
			if !ok {
				return fmt.Errorf("access.clients.%s: service %q is not defined in services", key, svc)
			}
		}
	}
	return nil
}
