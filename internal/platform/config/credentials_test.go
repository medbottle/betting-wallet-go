package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const credentialsDoc = `{
  "consumer":  {"accessKeyId": "AKIACONSUMER", "secretAccessKey": "consumer-file-secret"},
  "publisher": {"accessKeyId": "AKIAPUBLISHER", "secretAccessKey": "publisher-file-secret"},
  "operator":  {"accessKeyId": "AKIAOPERATOR", "secretAccessKey": "operator-file-secret",
                "roleArn": "arn:aws:iam::333333333333:role/wager-operator"},
  "providers": {"provider-a": {"accessKeyId": "AKIAPA", "secretAccessKey": "pa-secret",
                               "roleArn": "arn:aws:iam::111111111111:role/wager-producer"}}
}`

func writeCredentials(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sqs-credentials.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// withoutKeys is validVars without the publisher and consumer keys.
func withoutKeys() map[string]string {
	m := validVars()
	for _, k := range []string{"SQS_PUBLISHER_ACCESS_KEY_ID", "SQS_PUBLISHER_SECRET_ACCESS_KEY", "SQS_CONSUMER_ACCESS_KEY_ID", "SQS_CONSUMER_SECRET_ACCESS_KEY"} {
		delete(m, k)
	}
	return m
}

func TestCredentialsFileSuppliesKeys(t *testing.T) {
	m := withoutKeys()
	m["SQS_CREDENTIALS_FILE"] = writeCredentials(t, credentialsDoc)
	c, err := LoadFrom(m)
	if err != nil {
		t.Fatal(err)
	}
	if c.SQSPublisherAccessKeyID != "AKIAPUBLISHER" || c.SQSPublisherSecretAccessKey != "publisher-file-secret" ||
		c.SQSConsumerAccessKeyID != "AKIACONSUMER" || c.SQSConsumerSecretAccessKey != "consumer-file-secret" {
		t.Fatalf("keys not taken from the file: %+v", c)
	}
}

func TestEnvironmentKeysWinOverTheFile(t *testing.T) {
	m := validVars()
	m["SQS_CREDENTIALS_FILE"] = writeCredentials(t, credentialsDoc)
	c, err := LoadFrom(m)
	if err != nil {
		t.Fatal(err)
	}
	if c.SQSPublisherAccessKeyID != "publisher" || c.SQSConsumerAccessKeyID != "consumer" {
		t.Fatalf("environment keys replaced: %+v", c)
	}
}

func TestCredentialsFileErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		path, want string
	}{
		"missing file":     {filepath.Join(t.TempDir(), "absent.json"), "SQS_CREDENTIALS_FILE"},
		"invalid JSON":     {writeCredentials(t, `{"consumer": {"secretAccessKey": "leaked-secret"`), "invalid JSON"},
		"no publisher key": {writeCredentials(t, `{"consumer": {"accessKeyId": "a", "secretAccessKey": "b"}}`), "SQS_PUBLISHER_ACCESS_KEY_ID"},
	} {
		t.Run(name, func(t *testing.T) {
			m := withoutKeys()
			m["SQS_CREDENTIALS_FILE"] = tc.path
			_, err := LoadFrom(m)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "leaked-secret") {
				t.Fatalf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestReadBrokerCredentials(t *testing.T) {
	c, err := ReadBrokerCredentials(writeCredentials(t, credentialsDoc))
	if err != nil {
		t.Fatal(err)
	}
	if c.Operator.RoleARN != "arn:aws:iam::333333333333:role/wager-operator" || c.Providers["provider-a"].AccessKeyID != "AKIAPA" {
		t.Fatalf("parsed %+v", c)
	}
}
