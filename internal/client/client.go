// Package client implements `deploy-agent deploy`, the subcommand run by
// the pipeline script task. It submits a deploy
// request, polls until a terminal state, and maps the outcome to a process
// exit code so the pipeline step succeeds or fails accordingly.
package client

import (
	"time"
)

const (
	// DefaultConnectTimeout is how long one HTTP attempt may take.
	DefaultConnectTimeout = 10 * time.Second
	// DefaultPollInterval is the delay between deploy status polls.
	DefaultPollInterval = 2 * time.Second
	// DefaultPollTimeout is the total time budget for submit and polling.
	DefaultPollTimeout = 10 * time.Minute
	// DefaultConnectRetries is how many connection failures submit tolerates.
	DefaultConnectRetries = 3
	// DefaultConnectBackoff is the delay between submit retries.
	DefaultConnectBackoff = 2 * time.Second
)

// ExitCode is the process status Run reports to the pipeline.
type ExitCode int

const (
	// ExitSuccess means the deploy finished as success or noop.
	ExitSuccess ExitCode = 0
	// ExitDeployFailed means the agent ran the deploy and it failed.
	ExitDeployFailed ExitCode = 1
	// ExitRequestFailed means the request never became a deploy.
	ExitRequestFailed ExitCode = 2
	// ExitTimeout means the client stopped waiting before a terminal state.
	ExitTimeout ExitCode = 3
)

// Options configures a single Run invocation.
type Options struct {
	Endpoint      string
	Service       string
	Tag           string
	Digest        string
	CredentialPEM []byte
	// Token, if set, supplies the OIDC bearer token sent with every
	// request (RunOIDC).
	Token          TokenSource
	ConnectTimeout time.Duration
	PollInterval   time.Duration
	PollTimeout    time.Duration
	ConnectRetries int
	ConnectBackoff time.Duration
}

// withDefaults fills in zero-valued timing fields.
func (o Options) withDefaults() Options {
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = DefaultPollTimeout
	}
	if o.ConnectRetries <= 0 {
		o.ConnectRetries = DefaultConnectRetries
	}
	if o.ConnectBackoff <= 0 {
		o.ConnectBackoff = DefaultConnectBackoff
	}
	return o
}

// Summary is the JSON printed to stdout, and the caller-facing result of
// Run. Credentials never appear here.
type Summary struct {
	DeployID string   `json:"deploy_id,omitempty"`
	Service  string   `json:"service"`
	Tag      string   `json:"tag,omitempty"`
	Digest   string   `json:"digest,omitempty"`
	Result   string   `json:"result,omitempty"`
	Rollback string   `json:"rollback,omitempty"`
	ImageID  string   `json:"image_id,omitempty"`
	Message  string   `json:"message,omitempty"`
	ExitCode ExitCode `json:"exit_code"`
	Error    string   `json:"error,omitempty"`
}
