//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// keycloakImage must match docker-compose.yml.
const keycloakImage = "quay.io/keycloak/keycloak:26.7.5"

// Test identities from deploy/keycloak/realm-wagering.json.
var clientSecrets = map[string]string{
	"provider-a":      "provider-a-local-secret",
	"provider-b":      "provider-b-local-secret",
	"wallet-internal": "wallet-internal-local-secret",
}

// keycloakEnv is one real Keycloak shared by the package's tests: starting
// it takes tens of seconds. Without KC_HOSTNAME the issuer is the mapped
// URL the tests use (ARCHITECTURE §11, testcontainers note).
type keycloakEnv struct {
	BaseURL string
	Issuer  string
	JWKSURL string
}

var (
	kcOnce      sync.Once
	kcEnv       keycloakEnv
	kcErr       error
	kcContainer testcontainers.Container
)

func TestMain(m *testing.M) {
	code := m.Run()
	if kcContainer != nil {
		_ = kcContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

func startKeycloak(t *testing.T) keycloakEnv {
	t.Helper()
	kcOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		realm, err := filepath.Abs(filepath.Join(repoRoot(t), "deploy", "keycloak", "realm-wagering.json"))
		if err != nil {
			kcErr = err
			return
		}
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image: keycloakImage,
				Cmd:   []string{"start-dev", "--import-realm"},
				Env: map[string]string{
					"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
					"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
					"KC_HEALTH_ENABLED":           "true",
				},
				ExposedPorts: []string{"8080/tcp", "9000/tcp"},
				Files: []testcontainers.ContainerFile{{
					HostFilePath:      realm,
					ContainerFilePath: "/opt/keycloak/data/import/realm-wagering.json",
					FileMode:          0o644,
				}},
				WaitingFor: wait.ForHTTP("/health/ready").WithPort("9000/tcp").WithStartupTimeout(3 * time.Minute),
			},
			Started: true,
		})
		kcContainer = c
		if err != nil {
			kcErr = fmt.Errorf("start keycloak: %w", err)
			return
		}
		host, err := c.Host(ctx)
		if err != nil {
			kcErr = err
			return
		}
		port, err := c.MappedPort(ctx, "8080/tcp")
		if err != nil {
			kcErr = err
			return
		}
		base := fmt.Sprintf("http://%s:%s", host, port.Port())
		kcEnv = keycloakEnv{
			BaseURL: base,
			Issuer:  base + "/realms/wagering",
			JWKSURL: base + "/realms/wagering/protocol/openid-connect/certs",
		}
	})
	if kcErr != nil {
		t.Fatal(kcErr)
	}
	return kcEnv
}

// clientToken obtains an access token with client_credentials.
func clientToken(t *testing.T, kc keycloakEnv, clientID, secret string) (string, int) {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctxT(t), http.MethodPost, kc.Issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.AccessToken, resp.StatusCode
}

func mustToken(t *testing.T, kc keycloakEnv, clientID string) string {
	t.Helper()
	tok, code := clientToken(t, kc, clientID, clientSecrets[clientID])
	if code != http.StatusOK || tok == "" {
		t.Fatalf("token for %s: HTTP %d", clientID, code)
	}
	return tok
}
