package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validTOML = `
[webhook]
secret = "a-webhook-subscriber-secret-at-least-32-chars"

[gateway]
base_url = "http://host.docker.internal:9090/api/management/v1/"
username = "foo"
password = "bar"

[logging]
level = "debug"

[api_handles]
"Order API:v1.0" = "orderapi-1.0.0"
"cp-api-9f2a" = "echo-api-1-0-0"
`

// minimalTOML carries only what has no default, so a test can vary one thing.
const minimalTOML = `
[webhook]
secret = "a-secret-of-sufficient-length-here-ok"
[gateway]
username = "foo"
password = "bar"
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	return path
}

// isolateEnv clears every variable the loader reads, so a developer running the
// suite with SM_* exported in their shell gets the same result as CI. Empty
// counts as unset, and t.Setenv restores the previous value afterwards.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range recognizedEnvNames() {
		t.Setenv(name, "")
	}
}

func TestLoadReadsEveryField(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(write(t, validTOML))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Webhook.Secret != "a-webhook-subscriber-secret-at-least-32-chars" {
		t.Errorf("webhook.secret = %q", cfg.Webhook.Secret)
	}
	// The trailing slash must be trimmed, or every request path doubles it.
	if cfg.Gateway.BaseURL != "http://host.docker.internal:9090/api/management/v1" {
		t.Errorf("gateway.base_url = %q, want the trailing slash trimmed", cfg.Gateway.BaseURL)
	}
	if cfg.Gateway.Username != "foo" || cfg.Gateway.Password != "bar" {
		t.Errorf("gateway credentials = %q / %q", cfg.Gateway.Username, cfg.Gateway.Password)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("logging.level = %q", cfg.Logging.Level)
	}
	// api_handles keys are folded to lower case so lookups are case-insensitive.
	if got := cfg.APIHandles["order api:v1.0"]; got != "orderapi-1.0.0" {
		t.Errorf(`api_handles["Order API:v1.0"] did not fold: %q`, got)
	}
	if got := cfg.APIHandles["cp-api-9f2a"]; got != "echo-api-1-0-0" {
		t.Errorf("api_handles by ref_id = %q", got)
	}
}

// TestAPIHandleKeysWithDotsSurviveLoading pins the one koanf behaviour this
// config depends on: an api_handles key routinely contains a dot ("order
// api:v1.0"), and koanf is constructed with "." as its key delimiter. The key
// must come back whole rather than split into a nested map, or every
// version-qualified override silently stops resolving.
func TestAPIHandleKeysWithDotsSurviveLoading(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(write(t, minimalTOML+`
[api_handles]
"Order API:v1.0" = "orderapi-1.0.0"
"a.b.c" = "deeply.dotted"`))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := cfg.APIHandles["order api:v1.0"]; got != "orderapi-1.0.0" {
		t.Errorf(`api_handles["Order API:v1.0"] = %q, want the dotted key kept whole`, got)
	}
	if got := cfg.APIHandles["a.b.c"]; got != "deeply.dotted" {
		t.Errorf(`api_handles["a.b.c"] = %q, want the dotted key kept whole`, got)
	}
	if len(cfg.APIHandles) != 2 {
		t.Errorf("api_handles = %#v, want exactly the two configured keys", cfg.APIHandles)
	}
}

// TestDefaultsApplyWhenTheFileIsSilent covers the bottom layer. The gateway
// default has to be a host-published address: the mediator shares no network
// with the gateway, so a service name would not resolve.
func TestDefaultsApplyWhenTheFileIsSilent(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(write(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Gateway.BaseURL != DefaultGatewayBaseURL {
		t.Errorf("gateway.base_url = %q, want the default %q", cfg.Gateway.BaseURL, DefaultGatewayBaseURL)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("logging.level = %q, want the default info", cfg.Logging.Level)
	}
	if cfg.APIHandles == nil {
		t.Error("APIHandles is nil with no [api_handles] section; lookups would need a nil check")
	}
}

// TestEnvOverridesTheFile is the injection path: a container gets its settings
// without the mounted file being edited.
func TestEnvOverridesTheFile(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_WEBHOOK_SECRET", "the-secret-from-the-environment-ok")
	t.Setenv("SM_GATEWAY_BASE_URL", "https://gateway.example.com/api/management/v1/")
	t.Setenv("SM_GATEWAY_USERNAME", "env-user")
	t.Setenv("SM_GATEWAY_PASSWORD", "env-pass")
	t.Setenv("SM_LOG_LEVEL", "WARN")

	cfg, err := Load(write(t, validTOML))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Webhook.Secret != "the-secret-from-the-environment-ok" {
		t.Errorf("webhook.secret = %q, want the environment to win", cfg.Webhook.Secret)
	}
	if want := "https://gateway.example.com/api/management/v1"; cfg.Gateway.BaseURL != want {
		t.Errorf("gateway.base_url = %q, want %q", cfg.Gateway.BaseURL, want)
	}
	if cfg.Gateway.Username != "env-user" || cfg.Gateway.Password != "env-pass" {
		t.Errorf("gateway credentials = %q / %q, want the environment to win",
			cfg.Gateway.Username, cfg.Gateway.Password)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("logging.level = %q, want the environment value folded to warn", cfg.Logging.Level)
	}
	// The file still supplies what no variable covers.
	if got := cfg.APIHandles["order api:v1.0"]; got != "orderapi-1.0.0" {
		t.Errorf("api_handles = %#v, want the file's entries to survive the overlay", cfg.APIHandles)
	}
}

// TestEnvOnlyNeedsNoFile is the other injection shape: nothing mounted, every
// required value in the environment. A missing file is not itself an error.
func TestEnvOnlyNeedsNoFile(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_WEBHOOK_SECRET", "the-secret-from-the-environment-ok")
	t.Setenv("SM_GATEWAY_USERNAME", "env-user")
	t.Setenv("SM_GATEWAY_PASSWORD", "env-pass")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load() with no file but a complete environment = %v", err)
	}
	if cfg.Webhook.Secret != "the-secret-from-the-environment-ok" {
		t.Errorf("webhook.secret = %q", cfg.Webhook.Secret)
	}
	if cfg.Gateway.BaseURL != DefaultGatewayBaseURL {
		t.Errorf("gateway.base_url = %q, want the built-in default", cfg.Gateway.BaseURL)
	}
}

// TestEmptyEnvVarDoesNotBlankTheFile guards the compose passthrough: an empty
// entry in .env, or a variable listed in docker-compose.yaml that nobody set,
// must not erase a value the file supplied.
func TestEmptyEnvVarDoesNotBlankTheFile(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_GATEWAY_USERNAME", "")
	t.Setenv("SM_GATEWAY_BASE_URL", "   ")

	cfg, err := Load(write(t, validTOML))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Gateway.Username != "foo" {
		t.Errorf("gateway.username = %q, want the file value kept", cfg.Gateway.Username)
	}
	if cfg.Gateway.BaseURL != "http://host.docker.internal:9090/api/management/v1" {
		t.Errorf("gateway.base_url = %q, want the file value kept", cfg.Gateway.BaseURL)
	}
}

// TestUnknownEnvVarIsIgnored asserts a stray SM_* variable is a warning, not a
// startup failure — an operator's unrelated variable must not stop the service.
func TestUnknownEnvVarIsIgnored(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_NOT_A_SETTING", "whatever")

	if _, err := Load(write(t, validTOML)); err != nil {
		t.Fatalf("Load() with an unrecognized SM_ variable = %v, want it ignored", err)
	}
}

// TestMissingRequiredValuesFailFast checks each required setting is reported by
// name, with both places it can be supplied — the point of failing fast is that
// the message says what to do.
func TestMissingRequiredValuesFailFast(t *testing.T) {
	cases := map[string]struct {
		body      string
		wantTOML  string
		wantEnvar string
	}{
		"no secret": {`
[gateway]
username = "foo"
password = "bar"`, "secret", "SM_WEBHOOK_SECRET"},
		"no username": {`
[webhook]
secret = "a-secret-of-sufficient-length-here-ok"
[gateway]
password = "bar"`, "username", "SM_GATEWAY_USERNAME"},
		"no password": {`
[webhook]
secret = "a-secret-of-sufficient-length-here-ok"
[gateway]
username = "foo"`, "password", "SM_GATEWAY_PASSWORD"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)

			_, err := Load(write(t, tc.body))
			if err == nil {
				t.Fatalf("Load() with %s = nil, want a refusal to start", name)
			}
			if !strings.Contains(err.Error(), tc.wantTOML) {
				t.Errorf("error does not name the missing setting %q: %v", tc.wantTOML, err)
			}
			if !strings.Contains(err.Error(), tc.wantEnvar) {
				t.Errorf("error does not name the %s environment variable: %v", tc.wantEnvar, err)
			}
		})
	}
}

// TestMissingFileIsNamedWhenNothingElseSupplies covers the empty-handed start:
// no file, no environment. The error has to point at the file, since that is
// almost always what went wrong.
func TestMissingFileIsNamedWhenNothingElseSupplies(t *testing.T) {
	isolateEnv(t)

	path := filepath.Join(t.TempDir(), "absent.toml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() with neither a file nor environment values = nil, want an error")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the error does not say the config file is missing: %v", err)
	}
	if !strings.Contains(err.Error(), "SM_WEBHOOK_SECRET") {
		t.Errorf("the error does not offer the environment variable: %v", err)
	}
}

func TestLoadRejectsMalformedTOML(t *testing.T) {
	isolateEnv(t)

	if _, err := Load(write(t, "[webhook\nsecret = ")); err == nil {
		t.Fatal("Load() on malformed TOML = nil, want an error")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"file scheme": minimalTOML + `
[gateway]
base_url = "file:///etc/passwd"`,
		"no host": minimalTOML + `
[gateway]
base_url = "http://"`,
		"bad log level": minimalTOML + `
[logging]
level = "verbose"`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)

			if _, err := Load(write(t, body)); err == nil {
				t.Errorf("Load() with %s = nil, want an error", name)
			}
		})
	}
}

// TestInvalidEnvValueIsRejected asserts the overlay is validated like the file:
// an injected value cannot bypass the checks.
func TestInvalidEnvValueIsRejected(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_GATEWAY_BASE_URL", "file:///etc/passwd")

	if _, err := Load(write(t, validTOML)); err == nil {
		t.Fatal("Load() with a file:// URL in the environment = nil, want an error")
	}
}

// TestShippedTemplateDoesNotStart asserts the committed template is not a
// working configuration: the secret and credentials are blank, so an operator
// has to supply them — in the file or in the environment.
func TestShippedTemplateDoesNotStart(t *testing.T) {
	isolateEnv(t)

	shipped, err := os.ReadFile("../../configs/config-template.toml")
	if err != nil {
		t.Skipf("config-template.toml not readable from the test's directory: %v", err)
	}
	if _, err := Load(write(t, string(shipped))); err == nil {
		t.Fatal("Load() on the shipped config-template.toml = nil, want a refusal to start")
	}
}

// TestShippedTemplateGatewayDefault pins the address the template ships, which
// is what a three-stack deployment relies on: the gateway's published port
// reached through the host, not a name on a network this stack never joins.
func TestShippedTemplateGatewayDefault(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_WEBHOOK_SECRET", "a-secret-of-sufficient-length-here-ok")
	t.Setenv("SM_GATEWAY_USERNAME", "admin")
	t.Setenv("SM_GATEWAY_PASSWORD", "admin")

	shipped, err := os.ReadFile("../../configs/config-template.toml")
	if err != nil {
		t.Skipf("config-template.toml not readable from the test's directory: %v", err)
	}
	cfg, err := Load(write(t, string(shipped)))
	if err != nil {
		t.Fatalf("Load() with the required values injected = %v", err)
	}
	if cfg.Gateway.BaseURL != DefaultGatewayBaseURL {
		t.Errorf("the template's gateway.base_url = %q, want %q", cfg.Gateway.BaseURL, DefaultGatewayBaseURL)
	}
}

// TestShippedConfigCarriesNoSecrets guards the file compose mounts. It is
// tracked by git, so a filled-in copy must never be committed: as shipped it has
// to be missing its required values and therefore refuse to start. A commit that
// pastes a real secret in here fails this test.
func TestShippedConfigCarriesNoSecrets(t *testing.T) {
	isolateEnv(t)

	shipped, err := os.ReadFile("../../configs/config.toml")
	if err != nil {
		t.Skipf("config.toml not readable from the test's directory: %v", err)
	}
	if _, err := Load(write(t, string(shipped))); err == nil {
		t.Fatal("Load() on the shipped configs/config.toml = nil, want a refusal to start — " +
			"it must not carry a committed secret or credentials")
	}
}

// TestShippedConfigStartsOnceSupplied is the other half: with the required
// values injected, the mounted file is a working configuration and keeps the
// gateway default a three-stack deployment relies on.
func TestShippedConfigStartsOnceSupplied(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_WEBHOOK_SECRET", "a-secret-of-sufficient-length-here-ok")
	t.Setenv("SM_GATEWAY_USERNAME", "admin")
	t.Setenv("SM_GATEWAY_PASSWORD", "admin")

	shipped, err := os.ReadFile("../../configs/config.toml")
	if err != nil {
		t.Skipf("config.toml not readable from the test's directory: %v", err)
	}
	cfg, err := Load(write(t, string(shipped)))
	if err != nil {
		t.Fatalf("Load() with the required values injected = %v", err)
	}
	if cfg.Gateway.BaseURL != DefaultGatewayBaseURL {
		t.Errorf("the shipped gateway.base_url = %q, want %q", cfg.Gateway.BaseURL, DefaultGatewayBaseURL)
	}
}

func TestGatewayCAFileComesFromTheEnvironment(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_GATEWAY_CA_FILE", " /etc/subscription-mediator/gateway-ca/ca.crt ")

	cfg, err := Load(write(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.Gateway.CAFile != "/etc/subscription-mediator/gateway-ca/ca.crt" {
		t.Errorf("gateway.ca_file = %q, want the trimmed environment value", cfg.Gateway.CAFile)
	}
}

// TestGatewayCAFileNeedsAnHTTPSAddress asserts a CA that could never be used
// stops startup instead of being silently ignored.
func TestGatewayCAFileNeedsAnHTTPSAddress(t *testing.T) {
	isolateEnv(t)
	t.Setenv("SM_GATEWAY_BASE_URL", "http://gateway:9090/api/management/v1")
	t.Setenv("SM_GATEWAY_CA_FILE", "/etc/ca.crt")

	_, err := Load(write(t, minimalTOML))
	if err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("Load() with a CA and an http address = %v, want an error naming ca_file", err)
	}
}
