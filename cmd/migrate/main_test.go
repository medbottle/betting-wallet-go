package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestRunRejectsBadArgs(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
	}{
		{nil, "usage"},
		{[]string{"sideways"}, "usage"},
		{[]string{"up", "extra"}, "usage"},
		{[]string{"down", "0"}, "positive integer"},
		{[]string{"down", "x"}, "positive integer"},
		{[]string{"down", "1", "2"}, "usage"},
		{[]string{"up"}, "MIGRATE_DATABASE_URL"},
	}
	for _, tt := range tests {
		err := run(context.Background(), tt.args, "", io.Discard)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("run(%v) = %v, want error containing %q", tt.args, err, tt.wantErr)
		}
	}
}
