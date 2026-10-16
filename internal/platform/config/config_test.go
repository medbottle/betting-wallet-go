package config

import (
	"strings"
	"testing"
	"time"
)

func validVars() map[string]string {
	return map[string]string{
		"DATABASE_URL":  "postgres://wallet_app:x@localhost:5432/wallet",
		"OIDC_ISSUER":   "http://localhost:8080/realms/wagering",
		"OIDC_JWKS_URL": "http://keycloak:8080/realms/wagering/protocol/openid-connect/certs",

		"SQS_PUBLISHER_ACCESS_KEY_ID":     "publisher",
		"SQS_PUBLISHER_SECRET_ACCESS_KEY": "publisher-secret",
	}
}

func TestLoadFromDefaults(t *testing.T) {
	c, err := LoadFrom(validVars())
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.HTTPAddr != ":8081" {
		t.Errorf("HTTPAddr = %q, want :8081", c.HTTPAddr)
	}
	if c.DBLockTimeout != 5*time.Second {
		t.Errorf("DBLockTimeout = %v, want 5s", c.DBLockTimeout)
	}
	if c.OIDCAudience != "wallet-api" || c.OIDCJWKSMinRefreshInterval != 30*time.Second {
		t.Errorf("OIDC defaults = %q/%v, want wallet-api/30s", c.OIDCAudience, c.OIDCJWKSMinRefreshInterval)
	}
	if c.ReferenceTTL != 10*time.Minute || c.ReferenceBackoffBase != time.Second || c.ReferenceBackoffMax != time.Minute {
		t.Errorf("reference defaults = %v/%v/%v, want 10m/1s/60s (ARCHITECTURE §8)", c.ReferenceTTL, c.ReferenceBackoffBase, c.ReferenceBackoffMax)
	}
	if c.ReferenceWorkerConcurrency != 4 || c.ReferenceWorkerPollInterval != time.Second {
		t.Errorf("worker defaults = %d/%v, want 4/1s", c.ReferenceWorkerConcurrency, c.ReferenceWorkerPollInterval)
	}
	if c.OutboxLease != 30*time.Second || c.OutboxBatchSize != 10 || c.OutboxPublishTimeout != 5*time.Second ||
		c.OutboxBackoffBase != time.Second || c.OutboxBackoffMax != 5*time.Minute || c.OutboxMaxAttempts != 20 {
		t.Errorf("outbox defaults differ from ARCHITECTURE §9: %+v", c)
	}
	if c.OutboxPollInterval != time.Second || c.OutboxRelayConcurrency != 1 || c.SQSEventsQueue != "wallet-events.fifo" || c.AWSRegion != "us-east-1" {
		t.Errorf("relay defaults: %+v", c)
	}
	if c.HTTPHandlerTimeout != 10*time.Second {
		t.Errorf("HTTPHandlerTimeout = %v, want 10s", c.HTTPHandlerTimeout)
	}
	if c.ReadinessInterval != 2*time.Second || c.ReadinessDrainDelay != 2*time.Second {
		t.Errorf("readiness defaults = %v/%v, want 2s/2s", c.ReadinessInterval, c.ReadinessDrainDelay)
	}
	if !c.RunHTTP || !c.RunConsumer || !c.RunOutbox || !c.RunReferenceWorker {
		t.Errorf("all role flags should default to true: %+v", c)
	}
}

func TestLoadFromRoleFlags(t *testing.T) {
	vars := validVars()
	vars["RUN_CONSUMER"] = "false"
	vars["RUN_OUTBOX"] = "false"
	c, err := LoadFrom(vars)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !c.RunHTTP || c.RunConsumer || c.RunOutbox || !c.RunReferenceWorker {
		t.Errorf("unexpected role flags: %+v", c)
	}
}

func TestLoadFromRejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string
	}{
		{"missing database url", func(m map[string]string) { delete(m, "DATABASE_URL") }, "DATABASE_URL"},
		{"empty database url", func(m map[string]string) { m["DATABASE_URL"] = "" }, "DATABASE_URL"},
		{"bad duration", func(m map[string]string) { m["HTTP_HANDLER_TIMEOUT"] = "soon" }, "HTTP_HANDLER_TIMEOUT"},
		{"zero duration", func(m map[string]string) { m["READINESS_INTERVAL"] = "0s" }, "READINESS_INTERVAL"},
		{"negative drain delay", func(m map[string]string) { m["READINESS_DRAIN_DELAY"] = "-1s" }, "READINESS_DRAIN_DELAY"},
		{"check timeout above interval", func(m map[string]string) { m["READINESS_CHECK_TIMEOUT"] = "5s" }, "READINESS_CHECK_TIMEOUT"},
		{"bad log level", func(m map[string]string) { m["LOG_LEVEL"] = "verbose" }, "LOG_LEVEL"},
		{"zero lock timeout", func(m map[string]string) { m["DB_LOCK_TIMEOUT"] = "0s" }, "DB_LOCK_TIMEOUT"},
		{"missing issuer", func(m map[string]string) { delete(m, "OIDC_ISSUER") }, "OIDC_ISSUER"},
		{"relative jwks url", func(m map[string]string) { m["OIDC_JWKS_URL"] = "/certs" }, "OIDC_JWKS_URL"},
		{"empty audience", func(m map[string]string) { m["OIDC_AUDIENCE"] = " " }, "OIDC_AUDIENCE"},
		{"zero jwks interval", func(m map[string]string) { m["OIDC_JWKS_MIN_REFRESH_INTERVAL"] = "0s" }, "OIDC_JWKS_MIN_REFRESH_INTERVAL"},
		{"zero reference ttl", func(m map[string]string) { m["REFERENCE_TTL"] = "0s" }, "REFERENCE_TTL"},
		{"backoff base above max", func(m map[string]string) { m["REFERENCE_BACKOFF_BASE"] = "2m" }, "REFERENCE_BACKOFF_BASE"},
		{"zero workers", func(m map[string]string) { m["REFERENCE_WORKER_CONCURRENCY"] = "0" }, "REFERENCE_WORKER_CONCURRENCY"},
		{"zero poll interval", func(m map[string]string) { m["REFERENCE_WORKER_POLL_INTERVAL"] = "0s" }, "REFERENCE_WORKER_POLL_INTERVAL"},
		{"missing publisher key", func(m map[string]string) { delete(m, "SQS_PUBLISHER_ACCESS_KEY_ID") }, "SQS_PUBLISHER_ACCESS_KEY_ID"},
		{"relative sqs endpoint", func(m map[string]string) { m["SQS_ENDPOINT"] = "ministack:4566" }, "SQS_ENDPOINT"},
		{"standard events queue", func(m map[string]string) { m["SQS_EVENTS_QUEUE"] = "wallet-events" }, "SQS_EVENTS_QUEUE"},
		{"batch above 10", func(m map[string]string) { m["OUTBOX_BATCH_SIZE"] = "11" }, "OUTBOX_BATCH_SIZE"},
		{"publish timeout not below lease", func(m map[string]string) { m["OUTBOX_PUBLISH_TIMEOUT"] = "30s" }, "OUTBOX_PUBLISH_TIMEOUT"},
		{"zero max attempts", func(m map[string]string) { m["OUTBOX_MAX_ATTEMPTS"] = "0" }, "OUTBOX_MAX_ATTEMPTS"},
		{"zero pool size", func(m map[string]string) { m["DB_MAX_CONNS"] = "0" }, "DB_MAX_CONNS"},
		{"bad bool", func(m map[string]string) { m["RUN_HTTP"] = "maybe" }, "RUN_HTTP"},
		{"empty listen address", func(m map[string]string) { m["HTTP_ADDR"] = " " }, "HTTP_ADDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := validVars()
			tt.mutate(vars)
			_, err := LoadFrom(vars)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %s", err, tt.wantErr)
			}
		})
	}
}
