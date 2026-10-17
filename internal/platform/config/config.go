// Package config loads and validates the process configuration from
// environment variables. Validation runs at startup so a misconfigured
// instance fails fast instead of serving traffic.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the full process configuration. Every field maps to one
// environment variable; .env.example documents them all.
type Config struct {
	// DatabaseURL is the connection string for the application role
	// (wallet_app). Migrations use a separate owner connection.
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	// DBMaxConns caps the pgxpool size.
	DBMaxConns int32 `env:"DB_MAX_CONNS" envDefault:"10"`
	// DBStartupTimeout bounds the startup ping and schema version check.
	DBStartupTimeout time.Duration `env:"DB_STARTUP_TIMEOUT" envDefault:"10s"`
	// DBLockTimeout is the default lock_timeout of business transactions
	// (ARCHITECTURE §4).
	DBLockTimeout time.Duration `env:"DB_LOCK_TIMEOUT" envDefault:"5s"`
	// DBStatementTimeout is the default statement_timeout of every
	// transaction (ARCHITECTURE §4 and §24, Q1): twice the lock timeout, the
	// same 10s as a reference-worker iteration.
	DBStatementTimeout time.Duration `env:"DB_STATEMENT_TIMEOUT" envDefault:"10s"`

	// OIDCIssuer is the exact expected iss claim (ARCHITECTURE §11).
	OIDCIssuer string `env:"OIDC_ISSUER,required,notEmpty"`
	// OIDCJWKSURL is where signing keys are fetched; independent of the
	// issuer so the internal network can be used (ARCHITECTURE §11).
	OIDCJWKSURL string `env:"OIDC_JWKS_URL,required,notEmpty"`
	// OIDCAudience must be contained in the aud claim.
	OIDCAudience string `env:"OIDC_AUDIENCE" envDefault:"wallet-api"`
	// OIDCJWKSMinRefreshInterval is the minimum gap between JWKS fetches
	// triggered by an unknown kid (rate limit, ARCHITECTURE §11/§19).
	OIDCJWKSMinRefreshInterval time.Duration `env:"OIDC_JWKS_MIN_REFRESH_INTERVAL" envDefault:"30s"`

	// Reference resolution (ARCHITECTURE §8): deadline = created_at + TTL;
	// retries back off as min(base * 2^attempts, max) with full jitter.
	ReferenceTTL         time.Duration `env:"REFERENCE_TTL" envDefault:"10m"`
	ReferenceBackoffBase time.Duration `env:"REFERENCE_BACKOFF_BASE" envDefault:"1s"`
	ReferenceBackoffMax  time.Duration `env:"REFERENCE_BACKOFF_MAX" envDefault:"60s"`
	// ReferenceWorkerConcurrency is the number of worker goroutines per
	// instance (ARCHITECTURE §8).
	ReferenceWorkerConcurrency int `env:"REFERENCE_WORKER_CONCURRENCY" envDefault:"4"`
	// ReferenceWorkerPollInterval is how long an idle worker waits before
	// looking for due rows again (ARCHITECTURE §21).
	ReferenceWorkerPollInterval time.Duration `env:"REFERENCE_WORKER_POLL_INTERVAL" envDefault:"1s"`

	// SQS (ARCHITECTURE §10). SQSEndpoint overrides the AWS endpoint (the
	// emulator); empty uses AWS itself.
	SQSEndpoint    string `env:"SQS_ENDPOINT"`
	AWSRegion      string `env:"AWS_REGION" envDefault:"us-east-1"`
	SQSEventsQueue string `env:"SQS_EVENTS_QUEUE" envDefault:"wallet-events.fifo"`
	// Publisher credentials, separate from the consumer's (ARCHITECTURE §10).
	SQSPublisherAccessKeyID     string `env:"SQS_PUBLISHER_ACCESS_KEY_ID,required,notEmpty"`
	SQSPublisherSecretAccessKey string `env:"SQS_PUBLISHER_SECRET_ACCESS_KEY,required,notEmpty"`

	// MetricsAddr is the separate Prometheus listener (ARCHITECTURE §25,
	// G2); empty disables it.
	MetricsAddr string `env:"METRICS_ADDR" envDefault:":9090"`

	// Inbound queue and the consumer's own credentials (ARCHITECTURE §10).
	SQSInboundQueue            string `env:"SQS_INBOUND_QUEUE" envDefault:"wager-transactions.fifo"`
	SQSInboundDLQ              string `env:"SQS_INBOUND_DLQ" envDefault:"wager-transactions-dlq.fifo"`
	SQSConsumerAccessKeyID     string `env:"SQS_CONSUMER_ACCESS_KEY_ID,required,notEmpty"`
	SQSConsumerSecretAccessKey string `env:"SQS_CONSUMER_SECRET_ACCESS_KEY,required,notEmpty"`
	// SQSProviderSenders maps each producer's broker identity to the
	// provider its messages act for, as senderId=providerId pairs separated
	// by commas (ARCHITECTURE §10/§11). The sender id is the SenderId SQS
	// reports, up to any ':' (an IAM user or role unique id; locally a
	// MiniStack account). Read it with ProviderSenders.
	SQSProviderSenders string `env:"SQS_PROVIDER_SENDERS"`
	// SQSOperatorSenderID is the redrive operator's sender id: only its
	// messages may name their original sender (ARCHITECTURE §10).
	SQSOperatorSenderID string `env:"SQS_OPERATOR_SENDER_ID"`

	// Consumer (ARCHITECTURE §10; pause interval §23).
	ConsumerWaitSeconds    int32         `env:"CONSUMER_WAIT_SECONDS" envDefault:"20"`
	ConsumerMaxMessages    int32         `env:"CONSUMER_MAX_MESSAGES" envDefault:"10"`
	ConsumerHandlerTimeout time.Duration `env:"CONSUMER_HANDLER_TIMEOUT" envDefault:"15s"`
	ConsumerBackoffMax     time.Duration `env:"CONSUMER_BACKOFF_MAX" envDefault:"60s"`
	ConsumerPauseInterval  time.Duration `env:"CONSUMER_PAUSE_INTERVAL" envDefault:"1s"`

	// Outbox relay (ARCHITECTURE §9; poll interval and concurrency §22).
	OutboxLease            time.Duration `env:"OUTBOX_LEASE" envDefault:"30s"`
	OutboxBatchSize        int           `env:"OUTBOX_BATCH_SIZE" envDefault:"10"`
	OutboxPublishTimeout   time.Duration `env:"OUTBOX_PUBLISH_TIMEOUT" envDefault:"5s"`
	OutboxBackoffBase      time.Duration `env:"OUTBOX_BACKOFF_BASE" envDefault:"1s"`
	OutboxBackoffMax       time.Duration `env:"OUTBOX_BACKOFF_MAX" envDefault:"5m"`
	OutboxMaxAttempts      int           `env:"OUTBOX_MAX_ATTEMPTS" envDefault:"20"`
	OutboxPollInterval     time.Duration `env:"OUTBOX_POLL_INTERVAL" envDefault:"1s"`
	OutboxRelayConcurrency int           `env:"OUTBOX_RELAY_CONCURRENCY" envDefault:"1"`

	// HTTPAddr is the listen address of the HTTP server.
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8081"`
	// HTTPReadHeaderTimeout bounds reading request headers.
	HTTPReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"5s"`
	// HTTPHandlerTimeout bounds a single request handler (ARCHITECTURE §14).
	HTTPHandlerTimeout time.Duration `env:"HTTP_HANDLER_TIMEOUT" envDefault:"10s"`

	// ReadinessInterval is the period of the background readiness loop.
	ReadinessInterval time.Duration `env:"READINESS_INTERVAL" envDefault:"2s"`
	// ReadinessCheckTimeout bounds each individual readiness check.
	ReadinessCheckTimeout time.Duration `env:"READINESS_CHECK_TIMEOUT" envDefault:"1s"`
	// ReadinessDrainDelay is how long /health/ready reports draining before
	// components start shutting down (ARCHITECTURE §14).
	ReadinessDrainDelay time.Duration `env:"READINESS_DRAIN_DELAY" envDefault:"2s"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// Role flags (ARCHITECTURE §14). A single binary runs any subset.
	RunHTTP            bool `env:"RUN_HTTP" envDefault:"true"`
	RunConsumer        bool `env:"RUN_CONSUMER" envDefault:"true"`
	RunOutbox          bool `env:"RUN_OUTBOX" envDefault:"true"`
	RunReferenceWorker bool `env:"RUN_REFERENCE_WORKER" envDefault:"true"`
}

// Load reads the configuration from the process environment and validates it.
func Load() (Config, error) {
	return LoadFrom(environMap(os.Environ()))
}

// LoadFrom reads the configuration from the given variables and validates it.
func LoadFrom(vars map[string]string) (Config, error) {
	var c Config
	if err := env.ParseWithOptions(&c, env.Options{Environment: vars}); err != nil {
		return Config{}, fmt.Errorf("config: %w", withEnvNames(err))
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks semantic constraints that struct tags cannot express.
// All problems are reported together.
func (c Config) Validate() error {
	var errs []error
	if c.DBMaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be >= 1"))
	}
	for _, u := range []struct{ name, value string }{{"OIDC_ISSUER", c.OIDCIssuer}, {"OIDC_JWKS_URL", c.OIDCJWKSURL}} {
		if parsed, err := url.Parse(u.value); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an absolute http(s) URL", u.name))
		}
	}
	if strings.TrimSpace(c.OIDCAudience) == "" {
		errs = append(errs, errors.New("OIDC_AUDIENCE must not be empty"))
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		errs = append(errs, errors.New("HTTP_ADDR must not be empty"))
	}
	positive := []struct {
		name string
		d    time.Duration
	}{
		{"DB_STARTUP_TIMEOUT", c.DBStartupTimeout},
		{"DB_LOCK_TIMEOUT", c.DBLockTimeout},
		{"DB_STATEMENT_TIMEOUT", c.DBStatementTimeout},
		{"OIDC_JWKS_MIN_REFRESH_INTERVAL", c.OIDCJWKSMinRefreshInterval},
		{"REFERENCE_TTL", c.ReferenceTTL},
		{"REFERENCE_BACKOFF_BASE", c.ReferenceBackoffBase},
		{"REFERENCE_BACKOFF_MAX", c.ReferenceBackoffMax},
		{"REFERENCE_WORKER_POLL_INTERVAL", c.ReferenceWorkerPollInterval},
		{"OUTBOX_LEASE", c.OutboxLease},
		{"OUTBOX_PUBLISH_TIMEOUT", c.OutboxPublishTimeout},
		{"OUTBOX_BACKOFF_BASE", c.OutboxBackoffBase},
		{"OUTBOX_POLL_INTERVAL", c.OutboxPollInterval},
		{"CONSUMER_HANDLER_TIMEOUT", c.ConsumerHandlerTimeout},
		{"CONSUMER_BACKOFF_MAX", c.ConsumerBackoffMax},
		{"CONSUMER_PAUSE_INTERVAL", c.ConsumerPauseInterval},
		{"HTTP_READ_HEADER_TIMEOUT", c.HTTPReadHeaderTimeout},
		{"HTTP_HANDLER_TIMEOUT", c.HTTPHandlerTimeout},
		{"READINESS_INTERVAL", c.ReadinessInterval},
		{"READINESS_CHECK_TIMEOUT", c.ReadinessCheckTimeout},
	}
	for _, p := range positive {
		if p.d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be > 0", p.name))
		}
	}
	if c.ReadinessDrainDelay < 0 {
		errs = append(errs, errors.New("READINESS_DRAIN_DELAY must be >= 0"))
	}
	if c.SQSEndpoint != "" {
		if u, err := url.Parse(c.SQSEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("SQS_ENDPOINT must be an absolute http(s) URL"))
		}
	}
	if strings.TrimSpace(c.AWSRegion) == "" || strings.TrimSpace(c.SQSEventsQueue) == "" {
		errs = append(errs, errors.New("AWS_REGION and SQS_EVENTS_QUEUE must not be empty"))
	}
	if !strings.HasSuffix(c.SQSEventsQueue, ".fifo") {
		errs = append(errs, errors.New("SQS_EVENTS_QUEUE must be a FIFO queue (.fifo)"))
	}
	for _, q := range []struct{ name, value string }{{"SQS_INBOUND_QUEUE", c.SQSInboundQueue}, {"SQS_INBOUND_DLQ", c.SQSInboundDLQ}} {
		if !strings.HasSuffix(q.value, ".fifo") || len(q.value) <= len(".fifo") {
			errs = append(errs, fmt.Errorf("%s must be a FIFO queue (.fifo)", q.name))
		}
	}
	if c.RunConsumer {
		errs = append(errs, c.validateSenders()...)
	}
	// ReceiveMessage limits: wait 0-20s, 1-10 messages.
	if c.ConsumerWaitSeconds < 0 || c.ConsumerWaitSeconds > 20 {
		errs = append(errs, errors.New("CONSUMER_WAIT_SECONDS must be between 0 and 20"))
	}
	if c.ConsumerMaxMessages < 1 || c.ConsumerMaxMessages > 10 {
		errs = append(errs, errors.New("CONSUMER_MAX_MESSAGES must be between 1 and 10"))
	}
	// SendMessageBatch takes at most 10 entries.
	if c.OutboxBatchSize < 1 || c.OutboxBatchSize > 10 {
		errs = append(errs, errors.New("OUTBOX_BATCH_SIZE must be between 1 and 10"))
	}
	if c.OutboxMaxAttempts < 1 || c.OutboxRelayConcurrency < 1 {
		errs = append(errs, errors.New("OUTBOX_MAX_ATTEMPTS and OUTBOX_RELAY_CONCURRENCY must be >= 1"))
	}
	if c.OutboxBackoffBase > c.OutboxBackoffMax {
		errs = append(errs, errors.New("OUTBOX_BACKOFF_BASE must be <= OUTBOX_BACKOFF_MAX"))
	}
	if c.OutboxPublishTimeout >= c.OutboxLease {
		errs = append(errs, errors.New("OUTBOX_PUBLISH_TIMEOUT must be shorter than OUTBOX_LEASE"))
	}
	if c.ReferenceWorkerConcurrency < 1 {
		errs = append(errs, errors.New("REFERENCE_WORKER_CONCURRENCY must be >= 1"))
	}
	if c.ReferenceBackoffBase > c.ReferenceBackoffMax {
		errs = append(errs, errors.New("REFERENCE_BACKOFF_BASE must be <= REFERENCE_BACKOFF_MAX"))
	}
	if c.ReadinessCheckTimeout > c.ReadinessInterval {
		errs = append(errs, errors.New("READINESS_CHECK_TIMEOUT must be <= READINESS_INTERVAL"))
	}
	if _, err := c.SlogLevel(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// SlogLevel parses LogLevel into a slog.Level.
func (c Config) SlogLevel() (slog.Level, error) {
	var l slog.Level
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
		if err := l.UnmarshalText([]byte(c.LogLevel)); err != nil {
			return 0, fmt.Errorf("LOG_LEVEL: %w", err)
		}
		return l, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, error (got %q)", c.LogLevel)
	}
}

// providerIDFormat is the providers_id_format CHECK of the providers table.
var providerIDFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ProviderSenders parses SQS_PROVIDER_SENDERS into sender id -> provider
// id. A sender appears once; several senders may act for one provider
// (for example during a key rotation).
func (c Config) ProviderSenders() (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(c.SQSProviderSenders) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(c.SQSProviderSenders, ",") {
		sender, provider, ok := strings.Cut(pair, "=")
		switch {
		case !ok || !validSenderID(sender) || !providerIDFormat.MatchString(provider):
			return nil, fmt.Errorf("SQS_PROVIDER_SENDERS: %q is not senderId=providerId (sender: printable ASCII without spaces, ',', '=' or ':')", pair)
		case out[sender] != "":
			return nil, fmt.Errorf("SQS_PROVIDER_SENDERS: sender %q appears twice", sender)
		}
		out[sender] = provider
	}
	return out, nil
}

// validateSenders checks the broker identities the consumer trusts
// (ARCHITECTURE §10): at least one provider sender, and an operator that
// is not also a provider.
func (c Config) validateSenders() []error {
	senders, err := c.ProviderSenders()
	if err != nil {
		return []error{err}
	}
	var errs []error
	if len(senders) == 0 {
		errs = append(errs, errors.New("SQS_PROVIDER_SENDERS must map at least one sender when RUN_CONSUMER is true"))
	}
	switch {
	case !validSenderID(c.SQSOperatorSenderID):
		errs = append(errs, errors.New("SQS_OPERATOR_SENDER_ID must be set when RUN_CONSUMER is true (printable ASCII without spaces, ',', '=' or ':')"))
	case senders[c.SQSOperatorSenderID] != "":
		errs = append(errs, errors.New("SQS_OPERATOR_SENDER_ID must not also be a provider sender"))
	}
	return errs
}

// validSenderID: 1-128 printable ASCII characters without spaces and
// without the separators of SQS_PROVIDER_SENDERS or the ':' that starts the
// session part of a role's SenderId.
func validSenderID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e || s[i] == ',' || s[i] == '=' || s[i] == ':' {
			return false
		}
	}
	return true
}

// withEnvNames rewrites the library's parse errors, which name the Go
// struct field, so they name the environment variable instead.
func withEnvNames(err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	t := reflect.TypeFor[Config]()
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if errors.As(e, &pe) {
			if f, ok := t.FieldByName(pe.Name); ok {
				name, _, _ := strings.Cut(f.Tag.Get("env"), ",")
				e = fmt.Errorf("%s: %w", name, pe.Err)
			}
		}
		out = append(out, e)
	}
	return errors.Join(out...)
}

func environMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
