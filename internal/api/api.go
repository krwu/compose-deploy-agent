// Package api defines the HTTP contract shared by the deploy-agent server
// and client: request/response payloads, error codes, and result enums.
package api

import "time"

// Result is the terminal outcome of a deploy attempt.
type Result string

const (
	// ResultNoop means the service was already running the requested tag.
	ResultNoop Result = "noop"
	// ResultSuccess means the new tag is running and passed readiness.
	ResultSuccess Result = "success"
	// ResultPullFailed means the image could not be pulled or inspected.
	ResultPullFailed Result = "pull_failed"
	// ResultConfigInvalid means compose config failed the pre-flight check.
	ResultConfigInvalid Result = "config_invalid"
	// ResultStartFailed means the service did not become ready.
	ResultStartFailed Result = "start_failed"
	// ResultPostDeployFailed means the service became ready but its fixed
	// post_deploy command exited non-zero or timed out.
	ResultPostDeployFailed Result = "post_deploy_failed"
	// ResultInterrupted means the agent stopped while the deploy was in progress.
	ResultInterrupted Result = "interrupted"
)

// Rollback describes what happened to the rollback attempt, if any.
type Rollback string

const (
	// RollbackNone means no rollback was attempted.
	RollbackNone Rollback = "none"
	// RollbackRolledBack means the service was restored to an older tag.
	RollbackRolledBack Rollback = "rolled_back"
	// RollbackRollbackFailed means the restore attempt did not become ready.
	RollbackRollbackFailed Rollback = "rollback_failed"
)

// State is the lifecycle state of a deploy record.
type State string

const (
	// StateDeploying means the deploy is still running.
	StateDeploying State = "deploying"
	// StateSucceeded means the deploy finished successfully.
	StateSucceeded State = "succeeded"
	// StateFailed means the deploy finished unsuccessfully.
	StateFailed State = "failed"
)

// IsTerminal reports whether the state is a terminal state (deploy finished).
func (s State) IsTerminal() bool {
	return s == StateSucceeded || s == StateFailed
}

// ErrorCode enumerates the stable machine-readable error codes returned in
// error responses.
type ErrorCode string

const (
	// ErrInvalidParam means the request failed validation.
	ErrInvalidParam ErrorCode = "invalid_param"
	// ErrServiceNotFound means the service or deploy id is not known.
	ErrServiceNotFound ErrorCode = "service_not_found"
	// ErrCertNotRegistered means the client certificate fingerprint is unknown.
	ErrCertNotRegistered ErrorCode = "cert_not_registered"
	// ErrCertRevoked means the client certificate was revoked.
	ErrCertRevoked ErrorCode = "cert_revoked"
	// ErrCertNotAllowed means the client certificate is not bound to the
	// requested service (services.yaml access.clients).
	ErrCertNotAllowed ErrorCode = "cert_not_allowed"
	// ErrUnauthenticated means the OIDC listener received no valid bearer
	// token (missing, malformed, bad signature, wrong iss/aud, expired, or
	// already used). The response never says which check failed.
	ErrUnauthenticated ErrorCode = "unauthenticated"
	// ErrAccessDenied means a valid OIDC token is not bound to the
	// requested service. The response never says which claim mismatched.
	ErrAccessDenied ErrorCode = "access_denied"
	// ErrBodyTooLarge means the request body exceeded the size limit.
	ErrBodyTooLarge ErrorCode = "body_too_large"
	// ErrAlreadyDeploying means another deploy of the service is in progress.
	ErrAlreadyDeploying ErrorCode = "already_deploying"
	// ErrInternal means the agent hit an unexpected failure.
	ErrInternal ErrorCode = "internal_error"
)

// ErrorResponse is the JSON body returned for all non-2xx responses.
type ErrorResponse struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// DeployRequest is the body of POST /v1/deploy. Exactly one of Tag and
// Digest must be set; Digest ("sha256:<64 hex>") is only accepted by
// services configured with image_ref: digest, which in turn reject Tag.
type DeployRequest struct {
	Service string `json:"service"`
	Tag     string `json:"tag,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// DeployAccepted is the 202 response body of POST /v1/deploy.
type DeployAccepted struct {
	DeployID string `json:"deploy_id"`
	Service  string `json:"service"`
	Tag      string `json:"tag,omitempty"`
	Digest   string `json:"digest,omitempty"`
	State    State  `json:"state"`
}

// DeployStatus is the response body of GET /v1/deploys/{deploy_id}.
type DeployStatus struct {
	DeployID     string     `json:"deploy_id"`
	Service      string     `json:"service"`
	State        State      `json:"state"`
	Result       Result     `json:"result,omitempty"`
	Rollback     Rollback   `json:"rollback,omitempty"`
	RunningTag   string     `json:"running_tag,omitempty"`
	TargetTag    string     `json:"target_tag"`
	ImageID      string     `json:"image_id,omitempty"`
	RolledBackTo string     `json:"rolled_back_to,omitempty"`
	Message      string     `json:"message,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// RunningInfo describes the currently running version of a service, as
// derived from docker at query time.
type RunningInfo struct {
	Tag     string `json:"tag,omitempty"`
	ImageID string `json:"image_id,omitempty"`
}

// LastHealthy describes the most recently confirmed-healthy version.
type LastHealthy struct {
	Tag       string    `json:"tag"`
	ImageID   string    `json:"image_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LastDeploySummary is an abbreviated view of the most recent deploy for a
// service, embedded in ServiceStatus.
type LastDeploySummary struct {
	DeployID   string     `json:"deploy_id"`
	TargetTag  string     `json:"target_tag"`
	Result     Result     `json:"result,omitempty"`
	Rollback   Rollback   `json:"rollback,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ServiceStatus is the response body of GET /v1/status/{service}.
type ServiceStatus struct {
	Service     string             `json:"service"`
	Deploying   bool               `json:"deploying"`
	Running     RunningInfo        `json:"running"`
	EnvTag      string             `json:"env_tag,omitempty"`
	LastHealthy *LastHealthy       `json:"last_healthy,omitempty"`
	LastDeploy  *LastDeploySummary `json:"last_deploy,omitempty"`
}

// HealthzResponse is the response body of GET /healthz.
type HealthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	UptimeS int64  `json:"uptime_s"`
}
