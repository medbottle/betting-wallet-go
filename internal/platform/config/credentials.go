package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// BrokerKey is one broker identity's access key, and the role it assumes
// when it acts through one (ARCHITECTURE §10, §28).
type BrokerKey struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	RoleARN         string `json:"roleArn,omitempty"`
}

func (k BrokerKey) complete() bool { return k.AccessKeyID != "" && k.SecretAccessKey != "" }

// BrokerCredentials is the file deploy/ministack/init-queues.sh writes
// (SQS_CREDENTIALS_FILE): the access keys MiniStack generated for each
// role. In AWS the same file can be rendered from a secret store, or the
// SQS_*_ACCESS_KEY_ID variables used instead.
type BrokerCredentials struct {
	Consumer  BrokerKey            `json:"consumer"`
	Publisher BrokerKey            `json:"publisher"`
	Operator  BrokerKey            `json:"operator"`
	Providers map[string]BrokerKey `json:"providers"`
}

// ReadBrokerCredentials reads and parses a credentials file. It runs once
// at startup, like reading the environment, so it takes no context. Errors
// never contain a secret.
func ReadBrokerCredentials(path string) (BrokerCredentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return BrokerCredentials{}, fmt.Errorf("SQS_CREDENTIALS_FILE: %w", err)
	}
	var c BrokerCredentials
	if err := json.Unmarshal(b, &c); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return BrokerCredentials{}, fmt.Errorf("SQS_CREDENTIALS_FILE: invalid JSON at offset %d", syntax.Offset)
		}
		return BrokerCredentials{}, errors.New("SQS_CREDENTIALS_FILE: invalid credentials document")
	}
	return c, nil
}

// applyCredentialsFile fills the publisher and consumer keys the
// environment left empty from SQS_CREDENTIALS_FILE.
func (c *Config) applyCredentialsFile() error {
	if c.SQSCredentialsFile == "" {
		return nil
	}
	creds, err := ReadBrokerCredentials(c.SQSCredentialsFile)
	if err != nil {
		return err
	}
	fill := func(id, secret *string, k BrokerKey) {
		if *id == "" && *secret == "" && k.complete() {
			*id, *secret = k.AccessKeyID, k.SecretAccessKey
		}
	}
	fill(&c.SQSPublisherAccessKeyID, &c.SQSPublisherSecretAccessKey, creds.Publisher)
	fill(&c.SQSConsumerAccessKeyID, &c.SQSConsumerSecretAccessKey, creds.Consumer)
	return nil
}
