// Package config loads the mediator's configuration.
//
// Three layers, each overriding the one before it:
//
//	built-in defaults  ->  config.toml  ->  SM_* environment variables
//
// So a setting can be shipped in the file, or injected into the container as an
// environment variable, or both — the environment wins.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	tomlparser "github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// DefaultPath is where the container looks for the file. Override it with the
// -config flag.
const DefaultPath = "/app/config.toml"

// EnvPrefix namespaces every environment variable this service reads.
const EnvPrefix = "SM_"

// Fixed values that are not worth configuring for a demo.
const (
	// ListenAddress is where the webhook endpoint listens inside the container.
	// Map it to whatever host port you like in docker-compose.yaml.
	ListenAddress = "0.0.0.0:8085"

	// WebhookPath is the path component of the targetUrl you register with the
	// API Portal.
	WebhookPath = "/devportal/events"

	// StatePath is the durable state: identifier mappings, the processed-event
	// ledger, and the pending queue.
	StatePath = "/app/data/state.jsonl"

	// MaxBodyBytes caps a single delivery body. Events are a few KB.
	MaxBodyBytes = 1 << 20
)

// DefaultGatewayBaseURL is the address the mediator tries when nothing else says
// otherwise. The mediator runs on its own Docker network and shares none with
// the gateway, so the default is the host-published port a separate gateway
// compose stack exposes, reached through Docker Desktop's host.docker.internal.
// Every other layout — Docker Engine on Linux, a gateway on the host, one in
// Kubernetes — is a matter of setting gateway.base_url or SM_GATEWAY_BASE_URL.
const DefaultGatewayBaseURL = "http://host.docker.internal:9090/api/management/v1"

// envKeys maps each recognized environment variable to the config key it sets.
// It is explicit rather than derived from the key path: the paths contain
// underscores of their own (gateway.base_url), so a mechanical
// underscore-to-dot translation would be ambiguous.
var envKeys = map[string]string{
	"SM_WEBHOOK_SECRET":   "webhook.secret",
	"SM_GATEWAY_BASE_URL": "gateway.base_url",
	"SM_GATEWAY_USERNAME": "gateway.username",
	"SM_GATEWAY_PASSWORD": "gateway.password",
	"SM_LOG_LEVEL":        "logging.level",
}

// envKeyFor names the environment variable that sets a config key, for error
// messages that have to tell an operator both ways to supply a value.
func envKeyFor(configKey string) string {
	for envKey, mapped := range envKeys {
		if mapped == configKey {
			return envKey
		}
	}
	return ""
}

// Config is the resolved configuration, mirroring config.toml.
type Config struct {
	Webhook WebhookConfig `koanf:"webhook"`
	Gateway GatewayConfig `koanf:"gateway"`
	Logging LoggingConfig `koanf:"logging"`
	// APIHandles pins a portal API to a gateway API handle, for when matching on
	// name and version cannot tell which API is meant. Keys are
	// "<name>:<version>" or the portal's api ref_id, matched case-insensitively.
	// File-only: a map of arbitrary keys has no sensible environment form.
	//
	// A key here routinely contains a dot ("order api:v1.0"). koanf's "." key
	// delimiter does not split it: the parsed map is carried through as-is, and
	// only the flat key index joins on the delimiter. See
	// TestAPIHandleKeysWithDotsSurviveLoading.
	APIHandles map[string]string `koanf:"api_handles"`
}

// WebhookConfig configures the API Portal side.
type WebhookConfig struct {
	// Secret is the secret registered with the API Portal webhook subscriber. It
	// verifies each delivery's signature and derives the key that decrypts
	// subscription tokens and API key values, so the mediator cannot do its job
	// without it.
	Secret string `koanf:"secret"`
}

// GatewayConfig addresses the Gateway Controller management REST API.
type GatewayConfig struct {
	BaseURL  string `koanf:"base_url"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
}

// LoggingConfig configures the logger.
type LoggingConfig struct {
	// Level is debug, info, warn or error. Empty means info.
	Level string `koanf:"level"`
}

// defaultConfig is the bottom layer: what holds when neither the file nor the
// environment says otherwise. It deliberately carries no credentials — those are
// required inputs, so a missing one stops startup instead of falling back to
// something that would ship as a working default.
func defaultConfig() *Config {
	return &Config{
		Gateway: GatewayConfig{BaseURL: DefaultGatewayBaseURL},
		Logging: LoggingConfig{Level: "info"},
	}
}

// Load resolves the configuration from the defaults, the file at path, and the
// SM_* environment variables, in that order of increasing precedence, then
// validates it. It fails fast: any missing or malformed required setting is
// returned as an error naming both ways to supply it, and the caller refuses to
// start rather than run half-configured.
//
// path may be absent. Configuration can be injected entirely through the
// environment, so a missing file is only a problem if what it would have
// supplied is missing too — in which case the error says the file wasn't there.
func Load(path string) (*Config, error) {
	k := koanf.New(".")

	fileLoaded, err := loadFile(k, path)
	if err != nil {
		return nil, err
	}
	if err := loadEnv(k); err != nil {
		return nil, err
	}

	cfg := defaultConfig()
	// WeaklyTypedInput lets an environment variable's string value decode into a
	// typed field; a value that cannot be coerced fails here rather than being
	// silently dropped.
	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			TagName:          "koanf",
			WeaklyTypedInput: true,
			Result:           cfg,
			DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to apply configuration from %s: %w", describeSources(path, fileLoaded), err)
	}

	cfg.normalize()

	if err := cfg.validate(path, fileLoaded); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadFile parses path into k, reporting whether the file was there. A missing
// file is not an error on its own: the environment may carry everything. Any
// other failure — unreadable, malformed — is fatal, because a file that exists
// is a file the operator meant to be used.
func loadFile(k *koanf.Koanf, path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	if err := k.Load(file.Provider(path), tomlparser.Parser()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if errors.Is(err, fs.ErrPermission) {
			return false, fmt.Errorf("configuration file %s cannot be read: %w", path, err)
		}
		return false, fmt.Errorf("configuration file %s is not valid TOML: %w", path, err)
	}
	return true, nil
}

// loadEnv overlays the SM_* environment variables onto k, so an operator can
// inject any recognized setting into the container without touching the file.
//
// An empty value counts as unset, mirroring shell ${VAR:-} semantics: an empty
// entry in a .env file (or a compose passthrough for a variable nobody set)
// must not blank out what the file supplied. An SM_* variable that matches no
// known key is almost always a typo, so it is reported rather than ignored in
// silence — by name only, since the value may be a secret.
func loadEnv(k *koanf.Koanf) error {
	var unknown []string
	provider := env.ProviderWithValue(EnvPrefix, ".", func(name, value string) (string, any) {
		key, ok := envKeys[name]
		if !ok {
			unknown = append(unknown, name)
			return "", nil
		}
		if strings.TrimSpace(value) == "" {
			return "", nil
		}
		return key, value
	})
	if err := k.Load(provider, nil); err != nil {
		return fmt.Errorf("failed to read %s* environment variables: %w", EnvPrefix, err)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		slog.Warn("ignoring unrecognized environment variables",
			slog.String("names", strings.Join(unknown, ", ")),
			slog.String("recognized", strings.Join(recognizedEnvNames(), ", ")))
	}
	return nil
}

// recognizedEnvNames lists every environment variable this service reads, sorted
// so the warning above is stable.
func recognizedEnvNames() []string {
	names := make([]string, 0, len(envKeys))
	for name := range envKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// normalize trims and folds the values that are compared or concatenated later,
// so every downstream use sees one canonical form.
func (c *Config) normalize() {
	c.Webhook.Secret = strings.TrimSpace(c.Webhook.Secret)
	c.Gateway.BaseURL = strings.TrimRight(strings.TrimSpace(c.Gateway.BaseURL), "/")
	c.Gateway.Username = strings.TrimSpace(c.Gateway.Username)
	c.Gateway.Password = strings.TrimSpace(c.Gateway.Password)
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))

	// Resolution keys are matched case-insensitively, so fold them once here
	// rather than at every lookup.
	folded := make(map[string]string, len(c.APIHandles))
	for key, value := range c.APIHandles {
		if key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value); key != "" && value != "" {
			folded[key] = value
		}
	}
	c.APIHandles = folded
}

// validate checks the settings the mediator cannot work without, and reports the
// first failure with both ways to fix it. fileLoaded is threaded through so a
// missing required value can also say the config file wasn't found — the usual
// cause when nothing at all is configured.
func (c *Config) validate(path string, fileLoaded bool) error {
	if c.Webhook.Secret == "" {
		return missing(path, fileLoaded, "webhook.secret",
			"it verifies each delivery's signature and decrypts the subscription token, "+
				"and must match the secret registered with the API Portal subscriber")
	}
	if c.Gateway.BaseURL == "" {
		return missing(path, fileLoaded, "gateway.base_url",
			"it addresses the Gateway Controller management REST API, "+
				"e.g. \"http://host.docker.internal:9090/api/management/v1\"")
	}
	u, err := url.Parse(c.Gateway.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid configuration: gateway.base_url must be an http(s) URL with a host, got %q "+
			"(set it in %s or in %s)", c.Gateway.BaseURL, fileRef(path, fileLoaded), envKeyFor("gateway.base_url"))
	}
	if c.Gateway.Username == "" {
		return missing(path, fileLoaded, "gateway.username",
			"the management API requires a user with the admin role — "+
				"every subscription and subscription-plan operation is admin-scoped")
	}
	if c.Gateway.Password == "" {
		return missing(path, fileLoaded, "gateway.password",
			"the management API authenticates with HTTP basic auth")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid configuration: logging.level must be debug, info, warn or error, got %q "+
			"(set it in %s or in %s)", c.Logging.Level, fileRef(path, fileLoaded), envKeyFor("logging.level"))
	}
	return nil
}

// missing renders the error for a required setting that nothing supplied: what
// is missing, why it is needed, and the two places to put it.
func missing(path string, fileLoaded bool, configKey, why string) error {
	section, field, _ := strings.Cut(configKey, ".")
	return fmt.Errorf("missing required configuration: [%s] %s is not set — %s. "+
		"Set it under [%s] in %s, or set the %s environment variable",
		section, field, why, section, fileRef(path, fileLoaded), envKeyFor(configKey))
}

// fileRef describes the config file in an error message, calling out the case
// where it does not exist — the likeliest reason a required value is missing.
func fileRef(path string, fileLoaded bool) string {
	if path == "" {
		return "a config file (none was given; use -config <path>)"
	}
	if !fileLoaded {
		return fmt.Sprintf("%s (which does not exist — copy configs/config.toml from the repository)", path)
	}
	return path
}

// describeSources names what a failure was assembled from, so a decoding error
// points at the file, the environment, or both.
func describeSources(path string, fileLoaded bool) string {
	if fileLoaded {
		return fmt.Sprintf("%s and the %s* environment variables", path, EnvPrefix)
	}
	return fmt.Sprintf("the %s* environment variables", EnvPrefix)
}
