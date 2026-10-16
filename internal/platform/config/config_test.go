package config

import (
	"strings"
	"testing"
	"time"
)

func validVars() map[string]string {
	return map[string]string{"DATABASE_URL": "postgres://wallet_app:x@localhost:5432/wallet"}
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
