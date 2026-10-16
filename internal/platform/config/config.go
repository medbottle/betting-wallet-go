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
		{"OIDC_JWKS_MIN_REFRESH_INTERVAL", c.OIDCJWKSMinRefreshInterval},
		{"REFERENCE_TTL", c.ReferenceTTL},
		{"REFERENCE_BACKOFF_BASE", c.ReferenceBackoffBase},
		{"REFERENCE_BACKOFF_MAX", c.ReferenceBackoffMax},
		{"REFERENCE_WORKER_POLL_INTERVAL", c.ReferenceWorkerPollInterval},
		{"OUTBOX_LEASE", c.OutboxLease},
		{"OUTBOX_PUBLISH_TIMEOUT", c.OutboxPublishTimeout},
		{"OUTBOX_BACKOFF_BASE", c.OutboxBackoffBase},
		{"OUTBOX_POLL_INTERVAL", c.OutboxPollInterval},
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
