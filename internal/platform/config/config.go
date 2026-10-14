// Package config loads 12-factor configuration from environment variables.
//
// Only infrastructure wiring and feature flags live here. Business configuration (sources,
// categories, AI routes, editorial policies) is database-managed through admin APIs.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string // development | test | production
	ServiceName string

	HTTPAddr      string
	AdminHTTPAddr string // internal listener for /metrics and pprof-free health
	PublicBaseURL string
	CORSOrigins   []string
	// WebConsole serves the developer console at /console (default on outside production).
	WebConsole bool
	// DevMailpitURL lets /v1/dev/mailbox read from a local Mailpit (development only).
	DevMailpitURL string
	MaxBodyBytes  int64

	DatabaseURL   string
	DBMaxConns    int32
	RedisURL      string
	BusDriver     string // postgres | kafka
	KafkaBrokers  []string
	KafkaClientID string

	OpenSearchURL      string
	OpenSearchUser     string
	OpenSearchPassword string
	OpenSearchPrefix   string
	QdrantURL          string
	QdrantAPIKey       string
	QdrantPrefix       string

	StorageDriver string // filesystem | s3
	StorageDir    string
	S3Endpoint    string
	S3AccessKey   string
	S3SecretKey   string
	S3Bucket      string
	S3UseSSL      bool
	S3Region      string
	// S3PublicEndpoint is the host:port clients use to reach storage; presigned URLs are signed
	// for it. Empty means S3Endpoint.
	S3PublicEndpoint string
	StorageURLTTL    time.Duration

	Auth AuthConfig

	MailDriver   string // devsink | smtp | log-disabled
	MailFrom     string
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPStartTLS bool

	PushDriver        string // none | apns,fcm
	APNSKeyID         string
	APNSTeamID        string
	APNSTopic         string
	APNSKeyPEM        string
	APNSProduction    bool
	FCMProjectID      string
	FCMServiceAccount string // JSON

	AnalyticsDriver string // noop | postgres | clickhouse
	ClickHouseURL   string
	OTLPEndpoint    string
	LangfuseHost    string
	LangfusePublic  string
	LangfuseSecret  string

	CrawlUserAgent    string
	CrawlAllowPrivate bool // tests only; never in production
	CrawlLiveEnabled  bool
	FixtureDir        string
	FixtureTimeAnchor *time.Time // FIXTURE_TIME_ANCHOR (RFC3339); dev/test only

	WorkerConcurrency int
	SchedulerInterval time.Duration

	// SecretsEncryptionKey encrypts provider API keys stored in the DB (AES-256-GCM).
	SecretsEncryptionKey []byte
}

type AuthConfig struct {
	Issuer             string
	Audience           string
	AccessTTL          time.Duration
	RefreshTTL         time.Duration // absolute max lifetime of a refresh family
	SigningKeys        map[string]ed25519.PrivateKey
	ActiveKeyID        string
	CodeHMACSecret     []byte
	CodeTTL            time.Duration
	CodeMaxAttempts    int
	ResendMinInterval  time.Duration
	ResendHourlyCap    int
	ResetTTL           time.Duration
	Argon2MemoryKiB    uint32
	Argon2Iterations   uint32
	Argon2Parallelism  uint8
	TermsPolicyVersion string
	EphemeralKeys      bool // true when no keys configured in development
}

func (c *Config) IsProduction() bool  { return c.Env == "production" }
func (c *Config) IsDevelopment() bool { return c.Env == "development" }

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	var rangeErrs []error
	c := &Config{
		Env:           env("APP_ENV", "development"),
		ServiceName:   env("SERVICE_NAME", "newsintel"),
		HTTPAddr:      env("HTTP_ADDR", ":8080"),
		AdminHTTPAddr: env("ADMIN_HTTP_ADDR", ":9090"),
		PublicBaseURL: env("PUBLIC_BASE_URL", "http://localhost:8080"),
		CORSOrigins:   list("CORS_ALLOWED_ORIGINS", ""),
		DevMailpitURL: env("DEV_MAILPIT_URL", ""),
		WebConsole:    boolEnv("WEB_CONSOLE_ENABLED", env("APP_ENV", "development") != "production"),
		MaxBodyBytes:  int64(intEnv("HTTP_MAX_BODY_BYTES", 1<<20)),

		DatabaseURL:   env("DATABASE_URL", ""),
		DBMaxConns:    int32(boundedIntEnv("DB_MAX_CONNS", 20, 1, 1000, &rangeErrs)), // #nosec G115 -- bounded
		RedisURL:      env("REDIS_URL", ""),
		BusDriver:     env("BUS_DRIVER", "postgres"),
		KafkaBrokers:  list("KAFKA_BROKERS", ""),
		KafkaClientID: env("KAFKA_CLIENT_ID", "newsintel"),

		OpenSearchURL:      env("OPENSEARCH_URL", ""),
		OpenSearchUser:     env("OPENSEARCH_USER", ""),
		OpenSearchPassword: env("OPENSEARCH_PASSWORD", ""),
		OpenSearchPrefix:   env("OPENSEARCH_INDEX_PREFIX", "newsintel"),
		QdrantURL:          env("QDRANT_URL", ""),
		QdrantAPIKey:       env("QDRANT_API_KEY", ""),
		QdrantPrefix:       env("QDRANT_COLLECTION_PREFIX", "newsintel_"),

		StorageDriver:    env("STORAGE_DRIVER", "filesystem"),
		StorageDir:       env("STORAGE_DIR", "./var/storage"),
		S3Endpoint:       env("S3_ENDPOINT", ""),
		S3AccessKey:      env("S3_ACCESS_KEY", ""),
		S3SecretKey:      env("S3_SECRET_KEY", ""),
		S3Bucket:         env("S3_BUCKET", "newsintel"),
		S3UseSSL:         boolEnv("S3_USE_SSL", false),
		S3Region:         env("S3_REGION", "us-east-1"),
		S3PublicEndpoint: env("S3_PUBLIC_ENDPOINT", ""),
		StorageURLTTL:    durEnv("STORAGE_URL_TTL", 5*time.Minute),

		MailDriver:   env("MAIL_DRIVER", "devsink"),
		MailFrom:     env("MAIL_FROM", "News Intelligence <no-reply@localhost>"),
		SMTPHost:     env("SMTP_HOST", ""),
		SMTPPort:     intEnv("SMTP_PORT", 587),
		SMTPUser:     env("SMTP_USER", ""),
		SMTPPassword: env("SMTP_PASSWORD", ""),
		SMTPStartTLS: boolEnv("SMTP_STARTTLS", true),

		PushDriver:        env("PUSH_DRIVER", "none"),
		APNSKeyID:         env("APNS_KEY_ID", ""),
		APNSTeamID:        env("APNS_TEAM_ID", ""),
		APNSTopic:         env("APNS_TOPIC", ""),
		APNSKeyPEM:        env("APNS_KEY_PEM", ""),
		APNSProduction:    boolEnv("APNS_PRODUCTION", false),
		FCMProjectID:      env("FCM_PROJECT_ID", ""),
		FCMServiceAccount: env("FCM_SERVICE_ACCOUNT_JSON", ""),

		AnalyticsDriver: env("ANALYTICS_DRIVER", "postgres"),
		ClickHouseURL:   env("CLICKHOUSE_URL", ""),
		OTLPEndpoint:    env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		LangfuseHost:    env("LANGFUSE_HOST", ""),
		LangfusePublic:  env("LANGFUSE_PUBLIC_KEY", ""),
		LangfuseSecret:  env("LANGFUSE_SECRET_KEY", ""),

		CrawlUserAgent:    env("CRAWL_USER_AGENT", "NewsIntelBot/0.1 (+https://example.invalid/bot; contact: ops@example.invalid)"),
		CrawlAllowPrivate: boolEnv("CRAWL_ALLOW_PRIVATE_NETWORKS", false),
		CrawlLiveEnabled:  boolEnv("CRAWL_LIVE_ENABLED", false),
		FixtureDir:        env("FIXTURE_DIR", "./fixtures"),

		WorkerConcurrency: intEnv("WORKER_CONCURRENCY", 4),
		SchedulerInterval: durEnv("SCHEDULER_INTERVAL", 15*time.Second),
	}

	c.Auth = AuthConfig{
		Issuer:             env("AUTH_ISSUER", "newsintel"),
		Audience:           env("AUTH_AUDIENCE", "newsintel-api"),
		AccessTTL:          durEnv("AUTH_ACCESS_TTL", 15*time.Minute),
		RefreshTTL:         durEnv("AUTH_REFRESH_TTL", 30*24*time.Hour),
		CodeTTL:            durEnv("AUTH_CODE_TTL", 10*time.Minute),
		CodeMaxAttempts:    intEnv("AUTH_CODE_MAX_ATTEMPTS", 5),
		ResendMinInterval:  durEnv("AUTH_RESEND_MIN_INTERVAL", 60*time.Second),
		ResendHourlyCap:    intEnv("AUTH_RESEND_HOURLY_CAP", 5),
		ResetTTL:           durEnv("AUTH_RESET_TTL", 30*time.Minute),
		Argon2MemoryKiB:    uint32(boundedIntEnv("AUTH_ARGON2_MEMORY_KIB", 64*1024, 1024, 4*1024*1024, &rangeErrs)), // #nosec G115 -- bounded
		Argon2Iterations:   uint32(boundedIntEnv("AUTH_ARGON2_ITERATIONS", 3, 1, 100, &rangeErrs)),                  // #nosec G115 -- bounded
		Argon2Parallelism:  uint8(boundedIntEnv("AUTH_ARGON2_PARALLELISM", 2, 1, 64, &rangeErrs)),                   // #nosec G115 -- bounded
		TermsPolicyVersion: env("TERMS_POLICY_VERSION", "2026-10-01"),
	}

	errs := rangeErrs
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	switch c.BusDriver {
	case "postgres":
	case "kafka":
		if len(c.KafkaBrokers) == 0 {
			errs = append(errs, errors.New("KAFKA_BROKERS is required when BUS_DRIVER=kafka"))
		}
	default:
		errs = append(errs, fmt.Errorf("BUS_DRIVER must be postgres or kafka, got %q", c.BusDriver))
	}

	keys, active, err := parseSigningKeys(env("AUTH_SIGNING_KEYS", ""), env("AUTH_ACTIVE_KEY_ID", ""))
	if err != nil {
		errs = append(errs, err)
	}
	if len(keys) == 0 {
		if c.IsProduction() {
			errs = append(errs, errors.New("AUTH_SIGNING_KEYS is required in production"))
		} else {
			// Development/test only: ephemeral key; tokens become invalid on restart.
			_, priv, _ := ed25519.GenerateKey(nil)
			keys = map[string]ed25519.PrivateKey{"ephemeral": priv}
			active = "ephemeral"
			c.Auth.EphemeralKeys = true
		}
	}
	c.Auth.SigningKeys, c.Auth.ActiveKeyID = keys, active

	c.Auth.CodeHMACSecret, err = secretBytes("AUTH_CODE_HMAC_SECRET", c.IsProduction())
	if err != nil {
		errs = append(errs, err)
	}
	c.SecretsEncryptionKey, err = secretBytes("SECRETS_ENCRYPTION_KEY", c.IsProduction())
	if err != nil {
		errs = append(errs, err)
	}
	if v := env("FIXTURE_TIME_ANCHOR", ""); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		switch {
		case perr != nil:
			errs = append(errs, fmt.Errorf("FIXTURE_TIME_ANCHOR must be RFC3339: %w", perr))
		case c.IsProduction():
			errs = append(errs, errors.New("FIXTURE_TIME_ANCHOR must not be set in production"))
		default:
			c.FixtureTimeAnchor = &t
		}
	}
	if c.IsProduction() && c.CrawlAllowPrivate {
		errs = append(errs, errors.New("CRAWL_ALLOW_PRIVATE_NETWORKS must not be enabled in production"))
	}
	if c.IsProduction() && c.Auth.Argon2MemoryKiB < 19*1024 {
		errs = append(errs, errors.New("AUTH_ARGON2_MEMORY_KIB must be at least 19456 in production (OWASP minimum)"))
	}
	if c.IsProduction() && c.QdrantURL == "" {
		errs = append(errs, errors.New("QDRANT_URL is required in production (in-memory vectors are for development only)"))
	}
	if c.IsProduction() && c.MailDriver == "devsink" {
		errs = append(errs, errors.New("MAIL_DRIVER=devsink is not allowed in production"))
	}
	return c, errors.Join(errs...)
}

// parseSigningKeys parses "kid1:base64seed,kid2:base64seed" (32-byte Ed25519 seeds).
func parseSigningKeys(raw, active string) (map[string]ed25519.PrivateKey, string, error) {
	keys := map[string]ed25519.PrivateKey{}
	if strings.TrimSpace(raw) == "" {
		return keys, "", nil
	}
	first := ""
	for _, part := range strings.Split(raw, ",") {
		kid, b64, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || kid == "" {
			return nil, "", errors.New("AUTH_SIGNING_KEYS entries must be kid:base64seed")
		}
		seed, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, "", fmt.Errorf("AUTH_SIGNING_KEYS key %q must be a base64 32-byte seed", kid)
		}
		keys[kid] = ed25519.NewKeyFromSeed(seed)
		if first == "" {
			first = kid
		}
	}
	if active == "" {
		active = first
	}
	if _, ok := keys[active]; !ok {
		return nil, "", fmt.Errorf("AUTH_ACTIVE_KEY_ID %q not present in AUTH_SIGNING_KEYS", active)
	}
	return keys, active, nil
}

func secretBytes(name string, required bool) ([]byte, error) {
	v := os.Getenv(name)
	if v == "" {
		if required {
			return nil, fmt.Errorf("%s is required in production", name)
		}
		// Development: random per-process secret.
		b := make([]byte, 32)
		if _, err := randRead(b); err != nil {
			return nil, err
		}
		return b, nil
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(b) < 32 {
		return nil, fmt.Errorf("%s must be base64 of at least 32 bytes", name)
	}
	return b, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func intEnv(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// boundedIntEnv reads an integer and reports values outside [lo, hi] instead of letting them
// wrap when narrowed to a smaller type.
func boundedIntEnv(k string, def, lo, hi int, errs *[]error) int {
	n := intEnv(k, def)
	if n < lo || n > hi {
		*errs = append(*errs, fmt.Errorf("%s must be between %d and %d, got %d", k, lo, hi, n))
		return def
	}
	return n
}

func boolEnv(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func durEnv(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func list(k, def string) []string {
	v := env(k, def)
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
