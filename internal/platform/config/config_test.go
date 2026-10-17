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
		"SQS_CONSUMER_ACCESS_KEY_ID":      "consumer",
		"SQS_CONSUMER_SECRET_ACCESS_KEY":  "consumer-secret",
		"SQS_PROVIDER_SENDERS":            "111111111111=provider-a,222222222222=provider-b",
		"SQS_OPERATOR_SENDER_ID":          "333333333333",
	}
}

func TestProviderSenders(t *testing.T) {
	c, err := LoadFrom(validVars())
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	got, err := c.ProviderSenders()
	if err != nil || len(got) != 2 || got["111111111111"] != "provider-a" || got["222222222222"] != "provider-b" {
		t.Fatalf("senders = %v, %v", got, err)
	}

	// Several identities may act for one provider (key rotation); AWS
	// unique ids are accepted as they are.
	c.SQSProviderSenders = "AIDAEXAMPLEUSER01=provider-a,AROAEXAMPLEROLE01=provider-a"
	if got, err := c.ProviderSenders(); err != nil || got["AIDAEXAMPLEUSER01"] != "provider-a" || got["AROAEXAMPLEROLE01"] != "provider-a" {
		t.Fatalf("senders = %v, %v", got, err)
	}
}

func TestSendersOnlyRequiredForTheConsumer(t *testing.T) {
	vars := validVars()
	delete(vars, "SQS_PROVIDER_SENDERS")
	delete(vars, "SQS_OPERATOR_SENDER_ID")
	vars["RUN_CONSUMER"] = "false"
	if _, err := LoadFrom(vars); err != nil {
		t.Fatalf("no consumer, no senders needed: %v", err)
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
	if c.DBStatementTimeout != 10*time.Second {
		t.Errorf("DBStatementTimeout = %v, want 10s (ARCHITECTURE §24, Q1)", c.DBStatementTimeout)
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
	if c.SQSInboundQueue != "wager-transactions.fifo" || c.SQSInboundDLQ != "wager-transactions-dlq.fifo" || c.ConsumerWaitSeconds != 20 ||
		c.ConsumerMaxMessages != 10 || c.ConsumerHandlerTimeout != 15*time.Second || c.ConsumerBackoffMax != time.Minute || c.ConsumerPauseInterval != time.Second {
		t.Errorf("consumer defaults differ from ARCHITECTURE §10/§23: %+v", c)
	}
	if c.MetricsAddr != ":9090" {
		t.Errorf("MetricsAddr = %q, want :9090 (ARCHITECTURE §25)", c.MetricsAddr)
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
		{"zero statement timeout", func(m map[string]string) { m["DB_STATEMENT_TIMEOUT"] = "0s" }, "DB_STATEMENT_TIMEOUT"},
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
		{"missing consumer key", func(m map[string]string) { delete(m, "SQS_CONSUMER_ACCESS_KEY_ID") }, "SQS_CONSUMER_ACCESS_KEY_ID"},
		{"standard inbound queue", func(m map[string]string) { m["SQS_INBOUND_QUEUE"] = "wager-transactions" }, "SQS_INBOUND_QUEUE"},
		{"wait above 20", func(m map[string]string) { m["CONSUMER_WAIT_SECONDS"] = "21" }, "CONSUMER_WAIT_SECONDS"},
		{"zero messages", func(m map[string]string) { m["CONSUMER_MAX_MESSAGES"] = "0" }, "CONSUMER_MAX_MESSAGES"},
		{"zero handler timeout", func(m map[string]string) { m["CONSUMER_HANDLER_TIMEOUT"] = "0s" }, "CONSUMER_HANDLER_TIMEOUT"},
		{"no provider senders", func(m map[string]string) { delete(m, "SQS_PROVIDER_SENDERS") }, "SQS_PROVIDER_SENDERS"},
		{"sender without provider", func(m map[string]string) { m["SQS_PROVIDER_SENDERS"] = "111111111111" }, "SQS_PROVIDER_SENDERS"},
		{"sender with session part", func(m map[string]string) { m["SQS_PROVIDER_SENDERS"] = "AROAX:session=provider-a" }, "SQS_PROVIDER_SENDERS"},
		{"bad provider id", func(m map[string]string) { m["SQS_PROVIDER_SENDERS"] = "111111111111=provider a" }, "SQS_PROVIDER_SENDERS"},
		{"duplicate sender", func(m map[string]string) {
			m["SQS_PROVIDER_SENDERS"] = "111111111111=provider-a,111111111111=provider-b"
		}, "appears twice"},
		{"missing operator sender", func(m map[string]string) { delete(m, "SQS_OPERATOR_SENDER_ID") }, "SQS_OPERATOR_SENDER_ID"},
		{"operator is a provider", func(m map[string]string) { m["SQS_OPERATOR_SENDER_ID"] = "111111111111" }, "SQS_OPERATOR_SENDER_ID"},
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
