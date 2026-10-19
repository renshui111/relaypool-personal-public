// Package config loads relaypool settings from a JSON file with environment
// variable overrides.
//
// Precedence (lowest to highest):
//
//	built-in defaults < config file < environment variables
//
// Every duration is expressed as a Go duration string ("30m", "6h") so the
// config file stays readable; ParseDuration is used with a fallback so a typo
// degrades to the default instead of failing startup.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the root configuration object.
type Config struct {
	// Listen is the host:port to bind. Defaults to 127.0.0.1:8787 and must
	// never be widened to 0.0.0.0 unless the operator edits the file: the
	// whole point of this project is a loopback-only gateway.
	Listen string `json:"listen"`

	// DataDir holds the SQLite database and any provider state.
	DataDir string `json:"data_dir"`

	// RuntimeDir is a scratch directory for provider runtimes (Qoder CLI
	// needs a writable HOME per account). Kept separate so it can be tmpfs.
	RuntimeDir string `json:"runtime_dir"`

	// APIKey authenticates /v1/* and /admin/*. When empty at startup a
	// random key is generated, printed once, and persisted.
	APIKey string `json:"api_key"`

	// Routing selects the pool strategy: round-robin | weighted-round-robin
	// | fill-first.
	Routing string `json:"routing"`

	// CrossProviderPool lets a bare model ID (e.g. "glm-5.2") route across
	// every provider family. Prefixed IDs ("workbuddy/glm-5.2") always pin
	// one family regardless of this flag.
	CrossProviderPool bool `json:"cross_provider_pool"`

	// Pool tunes scheduling, cooldown and retry behaviour.
	Pool PoolConfig `json:"pool"`

	// Session tunes conversation stickiness.
	Session SessionConfig `json:"session"`

	// Upstream tunes outbound HTTP behaviour.
	Upstream UpstreamConfig `json:"upstream"`

	// Schedule tunes the background jobs.
	Schedule ScheduleConfig `json:"schedule"`

	// Logging tunes request persistence.
	Logging LoggingConfig `json:"logging"`

	// Prompts controls outbound system-prompt handling.
	Prompts PromptConfig `json:"prompts"`

	// Armor holds the optional system-prompt preset feature. It is OFF unless a
	// preset directory is configured, and even then a request must opt in.
	Armor ArmorConfig `json:"armor"`

	// Access controls how the gateway is exposed.
	Access AccessConfig `json:"access"`
}

// ArmorConfig configures the optional system-prompt presets.
//
// The feature is disabled by default by the simple mechanism of Dir being
// empty: with no presets loaded there is nothing to substitute, so the request
// path short-circuits and no caller can accidentally trigger it.
type ArmorConfig struct {
	// Dir is the directory holding preset files (one .md or .txt per preset,
	// named after the preset). Empty disables the feature entirely.
	Dir string `json:"dir"`
	// Enabled is the initial state of the global armor switch.
	//
	// It is only a SEED: once an operator toggles armor in the console the
	// stored value takes precedence, so the choice survives a restart. It exists
	// so a headless deployment can turn armor on without opening the console.
	//
	// Armor is ONE global on/off decision, not a per-account one. The preset is
	// chosen from the requested MODEL, and a single account serves many models,
	// so a per-account switch would armor models it was never chosen for.
	Enabled bool `json:"enabled"`
	// Default names the preset reported as the registry default. It is NOT a
	// request-time fallback: the preset is selected from the requested model,
	// and a model with no matching family gets nothing rather than this one.
	Default string `json:"default"`
	// AllowPerRequest permits a client to opt in through a request header.
	AllowPerRequest bool `json:"allow_per_request"`
}

// AccessConfig retains the legacy mode field only to reject nonlocal configurations.
type AccessConfig struct {
	Mode string `json:"mode"`
}

// PoolConfig holds scheduling knobs.
type PoolConfig struct {
	// MaxAttempts 独立限制单个请求最多尝试的账号数，包括首次调用。
	MaxAttempts int `json:"max_attempts"`
	// MaxInflight caps concurrent requests per account when the account has
	// no explicit override. Zero means unlimited.
	MaxInflight int `json:"max_inflight"`

	// BreakerThreshold is the number of consecutive same-kind failures
	// before the backoff ladder starts climbing.
	BreakerThreshold int `json:"breaker_threshold"`

	// BreakerCooldown is the base cooldown for a breaker trip. It is
	// multiplied by 2^level as the ladder climbs.
	BreakerCooldown time.Duration `json:"breaker_cooldown"`

	// BreakerCooldownMax caps the exponential cooldown.
	BreakerCooldownMax time.Duration `json:"breaker_cooldown_max"`

	// DefaultCooldown is used when an error carries no Retry-After hint and
	// the classifier has no kind-specific value.
	DefaultCooldown time.Duration `json:"default_cooldown"`

	// MaxRetryAfter clamps an upstream Retry-After so a hostile or buggy
	// value cannot freeze an account for days.
	MaxRetryAfter time.Duration `json:"max_retry_after"`

	// MinRateLimitCooldown is the floor for a rate-limit cooldown.
	MinRateLimitCooldown time.Duration `json:"min_rate_limit_cooldown"`

	// PreferExpiring 在同优先级和地区内优先使用即将到期的额度包。
	PreferExpiring bool `json:"prefer_expiring"`

	// ExpiringSoon 是额度到期优先窗口，不能用访问令牌到期时间代替额度到期时间。
	ExpiringSoon time.Duration `json:"expiring_soon"`
}

// SessionConfig holds stickiness knobs.
type SessionConfig struct {
	Enabled bool `json:"enabled"`
	// TTL is how long an idle binding survives. Each hit renews it.
	TTL time.Duration `json:"ttl"`
	// Capacity bounds the LRU. A conversation older than the least recently
	// used entry is evicted, which is what keeps memory flat over time.
	Capacity int `json:"capacity"`
}

// UpstreamConfig holds outbound HTTP knobs.
type UpstreamConfig struct {
	// Timeout is the whole-request timeout for non-streaming calls.
	Timeout time.Duration `json:"timeout"`
	// HeaderTimeout is how long to wait for response headers on a stream.
	HeaderTimeout time.Duration `json:"header_timeout"`
	// IdleTimeout is the read gap allowed between stream chunks.
	IdleTimeout time.Duration `json:"idle_timeout"`
	// ProxyURL routes outbound provider traffic through a proxy. Empty means
	// direct. Loopback targets always bypass it.
	ProxyURL string `json:"proxy_url"`
	// UserAgent overrides the provider default when non-empty.
	UserAgent string `json:"user_agent"`
	// PassthroughIP forwards the caller's remote address upstream. Off by
	// default: it leaks the client and can trip upstream risk control.
	PassthroughIP bool `json:"passthrough_ip"`
}

// ScheduleConfig holds background job knobs.
type ScheduleConfig struct {
	// CatalogRefresh is how often each account's model list is refreshed.
	CatalogRefresh time.Duration `json:"catalog_refresh"`
	// CheckinEnabled turns the daily check-in job on or off.
	CheckinEnabled bool `json:"checkin_enabled"`
	// CheckinHours are the local hours (0-23) at which to attempt check-in.
	CheckinHours []int `json:"checkin_hours"`
	// KeepaliveEnabled turns the token keep-alive job on or off.
	KeepaliveEnabled bool `json:"keepalive_enabled"`
	// KeepaliveHours are the local hours at which to keep tokens warm.
	KeepaliveHours []int `json:"keepalive_hours"`
	// CooldownGC is how often expired cooldowns are swept.
	CooldownGC time.Duration `json:"cooldown_gc"`
}

// LoggingConfig holds persistence knobs.
type LoggingConfig struct {
	// RequestLogEnabled stores one row per completed request.
	RequestLogEnabled bool `json:"request_log_enabled"`
	// RetentionDays prunes request logs older than this. Zero disables
	// pruning.
	RetentionDays int `json:"retention_days"`
	// ArchiveBodies stores prompt and completion text. Off by default
	// because those bodies routinely contain sensitive user content.
	ArchiveBodies bool `json:"archive_bodies"`
}

// PromptConfig controls outbound system-prompt handling.
type PromptConfig struct {
	// Mode is "passthrough" (send the client system prompt as-is) or
	// "replace" (substitute an operator-supplied prompt).
	Mode string `json:"mode"`
	// Text is the replacement prompt used when Mode is "replace".
	Text string `json:"text"`
}

// Default returns a fully-populated config with production-safe values.
// The listener is loopback-only and body archiving is off on purpose.
func Default() Config {
	return Config{
		Listen:            "127.0.0.1:8787",
		DataDir:           "./data",
		RuntimeDir:        "./run",
		Routing:           "round-robin",
		CrossProviderPool: false,
		Pool: PoolConfig{
			MaxAttempts:          4,
			MaxInflight:          4,
			BreakerThreshold:     3,
			BreakerCooldown:      30 * time.Minute,
			BreakerCooldownMax:   6 * time.Hour,
			DefaultCooldown:      15 * time.Second,
			MaxRetryAfter:        10 * time.Minute,
			MinRateLimitCooldown: 30 * time.Second,
			PreferExpiring:       true,
			ExpiringSoon:         168 * time.Hour,
		},
		Session: SessionConfig{
			Enabled:  true,
			TTL:      30 * time.Minute,
			Capacity: 10000,
		},
		Upstream: UpstreamConfig{
			Timeout:       120 * time.Second,
			HeaderTimeout: 120 * time.Second,
			IdleTimeout:   300 * time.Second,
		},
		Schedule: ScheduleConfig{
			CatalogRefresh:   30 * time.Minute,
			CheckinEnabled:   true,
			CheckinHours:     []int{9, 21},
			KeepaliveEnabled: true,
			KeepaliveHours:   []int{22},
			CooldownGC:       5 * time.Minute,
		},
		Logging: LoggingConfig{
			RequestLogEnabled: true,
			RetentionDays:     7,
			ArchiveBodies:     false,
		},
		Prompts: PromptConfig{Mode: "passthrough"},
		Armor: ArmorConfig{
			// Dir defaults to the repo-relative preset directory so a source
			// checkout picks up the bundled preset without configuration. An
			// installed binary that lacks the directory simply loads none.
			Dir:             "prompts",
			AllowPerRequest: true,
		},
		Access: AccessConfig{Mode: AccessLoopback},
	}
}

// Access modes.
const (
	AccessLoopback = "loopback"
)

// durationField decodes a duration that may be written either as a Go duration
// string ("30m") or as a bare number of seconds.
//
// It OWNS its value rather than pointing at an external target. The earlier
// design held a *time.Duration into the destination struct, which cannot work
// with encoding/json: the decoder allocates the field from the JSON shape and
// never learns about an unexported pointer, so UnmarshalJSON dereferenced nil
// and panicked. A value-typed wrapper with an explicit Accessor is both correct
// and simpler.
type durationField struct {
	// Set reports whether the key was present in the JSON. It is what makes an
	// explicit zero distinguishable from an absent key, so "ttl": "0s" can
	// override a non-zero default while omitting ttl leaves it alone.
	Set   bool
	Value time.Duration
}

func (d *durationField) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	// Quoted string form: "30m", "6h".
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = strings.TrimSpace(unquoted)
		if text == "" {
			// An explicit empty string means "leave unset" rather than an
			// error, so a config template can carry empty placeholders.
			return nil
		}
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", text, err)
		}
		d.Set, d.Value = true, parsed
		return nil
	}
	// Bare number form: seconds.
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	d.Set, d.Value = true, time.Duration(seconds*float64(time.Second))
	return nil
}

// fileConfig mirrors Config but routes every duration through durationField,
// because encoding/json cannot decode "30m" into a time.Duration on its own.
type fileConfig struct {
	Listen            *string       `json:"listen"`
	DataDir           *string       `json:"data_dir"`
	RuntimeDir        *string       `json:"runtime_dir"`
	APIKey            *string       `json:"api_key"`
	Routing           *string       `json:"routing"`
	CrossProviderPool *bool         `json:"cross_provider_pool"`
	Pool              *poolFile     `json:"pool"`
	Session           *sessionFile  `json:"session"`
	Upstream          *upstreamFile `json:"upstream"`
	Schedule          *scheduleFile `json:"schedule"`
	Logging           *loggingFile  `json:"logging"`
	Prompts           *promptFile   `json:"prompts"`
	Armor             *armorFile    `json:"armor"`
	Access            *accessFile   `json:"access"`
}

type armorFile struct {
	Dir             *string `json:"dir"`
	Enabled         *bool   `json:"enabled"`
	Default         *string `json:"default"`
	AllowPerRequest *bool   `json:"allow_per_request"`
}

type accessFile struct {
	Mode *string `json:"mode"`
}

type poolFile struct {
	MaxAttempts          *int           `json:"max_attempts"`
	MaxInflight          *int           `json:"max_inflight"`
	BreakerThreshold     *int           `json:"breaker_threshold"`
	BreakerCooldown      *durationField `json:"breaker_cooldown"`
	BreakerCooldownMax   *durationField `json:"breaker_cooldown_max"`
	DefaultCooldown      *durationField `json:"default_cooldown"`
	MaxRetryAfter        *durationField `json:"max_retry_after"`
	MinRateLimitCooldown *durationField `json:"min_rate_limit_cooldown"`
	PreferExpiring       *bool          `json:"prefer_expiring"`
	ExpiringSoon         *durationField `json:"expiring_soon"`
}

type sessionFile struct {
	Enabled  *bool          `json:"enabled"`
	TTL      *durationField `json:"ttl"`
	Capacity *int           `json:"capacity"`
}

type upstreamFile struct {
	Timeout       *durationField `json:"timeout"`
	HeaderTimeout *durationField `json:"header_timeout"`
	IdleTimeout   *durationField `json:"idle_timeout"`
	ProxyURL      *string        `json:"proxy_url"`
	UserAgent     *string        `json:"user_agent"`
	PassthroughIP *bool          `json:"passthrough_ip"`
}

type scheduleFile struct {
	CatalogRefresh   *durationField `json:"catalog_refresh"`
	CheckinEnabled   *bool          `json:"checkin_enabled"`
	CheckinHours     *[]int         `json:"checkin_hours"`
	KeepaliveEnabled *bool          `json:"keepalive_enabled"`
	KeepaliveHours   *[]int         `json:"keepalive_hours"`
	CooldownGC       *durationField `json:"cooldown_gc"`
}

type loggingFile struct {
	RequestLogEnabled *bool `json:"request_log_enabled"`
	RetentionDays     *int  `json:"retention_days"`
	ArchiveBodies     *bool `json:"archive_bodies"`
}

type promptFile struct {
	Mode *string `json:"mode"`
	Text *string `json:"text"`
}

// Load merges defaults, then the JSON file at path (when it exists), then
// environment overrides. A missing file is not an error: defaults plus
// environment are a valid configuration.
func Load(path string) (Config, error) {
	cfg := Default()
	if strings.TrimSpace(path) != "" {
		if raw, err := os.ReadFile(path); err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("read config %s: %w", path, err)
			}
		} else if err := applyFile(&cfg, raw); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyFile(cfg *Config, raw []byte) error {
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return err
	}
	setString(&cfg.Listen, fc.Listen)
	setString(&cfg.DataDir, fc.DataDir)
	setString(&cfg.RuntimeDir, fc.RuntimeDir)
	setString(&cfg.APIKey, fc.APIKey)
	setString(&cfg.Routing, fc.Routing)
	setBool(&cfg.CrossProviderPool, fc.CrossProviderPool)

	if p := fc.Pool; p != nil {
		setInt(&cfg.Pool.MaxAttempts, p.MaxAttempts)
		setInt(&cfg.Pool.MaxInflight, p.MaxInflight)
		setInt(&cfg.Pool.BreakerThreshold, p.BreakerThreshold)
		setDuration(&cfg.Pool.BreakerCooldown, p.BreakerCooldown)
		setDuration(&cfg.Pool.BreakerCooldownMax, p.BreakerCooldownMax)
		setDuration(&cfg.Pool.DefaultCooldown, p.DefaultCooldown)
		setDuration(&cfg.Pool.MaxRetryAfter, p.MaxRetryAfter)
		setDuration(&cfg.Pool.MinRateLimitCooldown, p.MinRateLimitCooldown)
		setBool(&cfg.Pool.PreferExpiring, p.PreferExpiring)
		setDuration(&cfg.Pool.ExpiringSoon, p.ExpiringSoon)
	}
	if s := fc.Session; s != nil {
		setBool(&cfg.Session.Enabled, s.Enabled)
		setDuration(&cfg.Session.TTL, s.TTL)
		setInt(&cfg.Session.Capacity, s.Capacity)
	}
	if u := fc.Upstream; u != nil {
		setDuration(&cfg.Upstream.Timeout, u.Timeout)
		setDuration(&cfg.Upstream.HeaderTimeout, u.HeaderTimeout)
		setDuration(&cfg.Upstream.IdleTimeout, u.IdleTimeout)
		setString(&cfg.Upstream.ProxyURL, u.ProxyURL)
		setString(&cfg.Upstream.UserAgent, u.UserAgent)
		setBool(&cfg.Upstream.PassthroughIP, u.PassthroughIP)
	}
	if sc := fc.Schedule; sc != nil {
		setDuration(&cfg.Schedule.CatalogRefresh, sc.CatalogRefresh)
		setBool(&cfg.Schedule.CheckinEnabled, sc.CheckinEnabled)
		setIntSlice(&cfg.Schedule.CheckinHours, sc.CheckinHours)
		setBool(&cfg.Schedule.KeepaliveEnabled, sc.KeepaliveEnabled)
		setIntSlice(&cfg.Schedule.KeepaliveHours, sc.KeepaliveHours)
		setDuration(&cfg.Schedule.CooldownGC, sc.CooldownGC)
	}
	if lg := fc.Logging; lg != nil {
		setBool(&cfg.Logging.RequestLogEnabled, lg.RequestLogEnabled)
		setInt(&cfg.Logging.RetentionDays, lg.RetentionDays)
		setBool(&cfg.Logging.ArchiveBodies, lg.ArchiveBodies)
	}
	if pr := fc.Prompts; pr != nil {
		setString(&cfg.Prompts.Mode, pr.Mode)
		setString(&cfg.Prompts.Text, pr.Text)
	}
	if ar := fc.Armor; ar != nil {
		// Dir is set even when empty, because an explicit "" is how an operator
		// turns the feature off in a file that otherwise enables it.
		if ar.Dir != nil {
			cfg.Armor.Dir = strings.TrimSpace(*ar.Dir)
		}
		setString(&cfg.Armor.Default, ar.Default)
		setBool(&cfg.Armor.Enabled, ar.Enabled)
		setBool(&cfg.Armor.AllowPerRequest, ar.AllowPerRequest)
	}
	if ac := fc.Access; ac != nil {
		setString(&cfg.Access.Mode, ac.Mode)
	}
	return nil
}

// applyEnv lets the deployment override anything without editing the file.
// Only non-empty values win, so an unset variable never clobbers the file.
func applyEnv(cfg *Config) {
	setEnvString(&cfg.Listen, "RELAYPOOL_LISTEN")
	setEnvString(&cfg.DataDir, "RELAYPOOL_DATA_DIR")
	setEnvString(&cfg.RuntimeDir, "RELAYPOOL_RUNTIME_DIR")
	setEnvString(&cfg.APIKey, "RELAYPOOL_API_KEY")
	setEnvString(&cfg.Routing, "RELAYPOOL_ROUTING")
	setBool(&cfg.CrossProviderPool, envBool("RELAYPOOL_CROSS_PROVIDER_POOL"))
	setEnvString(&cfg.Upstream.ProxyURL, "RELAYPOOL_PROXY_URL")
	setEnvString(&cfg.Armor.Dir, "RELAYPOOL_ARMOR_DIR")
	setEnvString(&cfg.Armor.Default, "RELAYPOOL_ARMOR_DEFAULT")
	setEnvBool(&cfg.Armor.Enabled, "RELAYPOOL_ARMOR_ENABLED")
	setEnvString(&cfg.Access.Mode, "RELAYPOOL_ACCESS_MODE")
	// Honour the conventional proxy variables too, so an operator can point
	// the container at a host proxy without learning our own names.
	if strings.TrimSpace(cfg.Upstream.ProxyURL) == "" {
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if v := envString(name); v != "" {
				cfg.Upstream.ProxyURL = v
				break
			}
		}
	}
}

func envString(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// setEnvString assigns only when the variable is set and non-empty.
func setEnvString(target *string, name string) {
	if v := envString(name); v != "" {
		*target = v
	}
}

// setEnvBool applies an environment boolean when it is present and parseable.
// An unset or malformed variable leaves the target alone, so a typo cannot
// silently flip a switch off.
func setEnvBool(target *bool, name string) {
	if v := envBool(name); v != nil {
		*target = *v
	}
}

func envBool(name string) *bool {
	raw := envString(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &v
}

func setString(target *string, value *string) {
	if value != nil && strings.TrimSpace(*value) != "" {
		*target = *value
	}
}

func setBool(target *bool, value *bool) {
	if value != nil {
		*target = *value
	}
}

func setInt(target *int, value *int) {
	if value != nil {
		*target = *value
	}
}

// setDuration assigns a decoded duration, respecting presence.
//
// A key that was absent leaves the target at its default; a key present with an
// explicit zero sets zero. That distinction is what lets "ttl": "0s" disable a
// feature while omitting the key keeps the default in force.
func setDuration(target *time.Duration, value *durationField) {
	if value != nil && value.Set {
		*target = value.Value
	}
}

func setIntSlice(target *[]int, value *[]int) {
	if value != nil {
		*target = append([]int(nil), (*value)...)
	}
}

func setStringSlice(target *[]string, value *[]string) {
	if value != nil {
		*target = append([]string(nil), (*value)...)
	}
}

// ValidRoutingStrategies lists the accepted Routing values.
var ValidRoutingStrategies = []string{"round-robin", "weighted-round-robin", "fill-first"}

// Validate rejects combinations that would silently do the wrong thing.
func (c *Config) Validate() error {
	if c.Logging.RetentionDays < 0 || c.Logging.RetentionDays > 3650 {
		return fmt.Errorf("日志保留天数须在 0 到 3650 之间；0 表示关闭清理")
	}
	if strings.TrimSpace(c.Listen) == "" {
		return fmt.Errorf("listen must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q is not host:port: %w", c.Listen, err)
	}
	if !IsLoopbackListen(c.Listen) {
		// Binding a wildcard address is legitimate INSIDE a container, where
		// the container's own network namespace is the trust boundary and the
		// compose file controls the host-side binding. It is dangerous on a
		// bare host, so outside a container it must be asked for explicitly.
		if !inContainer() {
			return fmt.Errorf(
				"listen %q is not loopback; set RELAYPOOL_ALLOW_NON_LOOPBACK=1 to allow exposing the gateway",
				c.Listen)
		}
	}
	switch c.Routing {
	case "round-robin", "weighted-round-robin", "fill-first":
	default:
		return fmt.Errorf("routing %q must be one of %s", c.Routing, strings.Join(ValidRoutingStrategies, ", "))
	}
	if c.Pool.BreakerThreshold < 1 {
		return fmt.Errorf("pool.breaker_threshold must be >= 1")
	}
	if c.Pool.MaxAttempts < 1 || c.Pool.MaxAttempts > 8 {
		return fmt.Errorf("pool.max_attempts 必须在 1 到 8 之间")
	}
	if c.Pool.MaxInflight < 0 {
		return fmt.Errorf("pool.max_inflight 不能为负数")
	}
	if c.Pool.DefaultCooldown <= 0 || c.Pool.MinRateLimitCooldown <= 0 || c.Pool.BreakerCooldown <= 0 || c.Pool.BreakerCooldownMax <= 0 {
		return fmt.Errorf("号池冷却时间必须大于零")
	}
	if c.Pool.MinRateLimitCooldown > c.Pool.MaxRetryAfter {
		return fmt.Errorf("pool.min_rate_limit_cooldown 不能超过 pool.max_retry_after")
	}
	if c.Pool.MaxRetryAfter <= 0 {
		return fmt.Errorf("pool.max_retry_after must be > 0")
	}
	if c.Pool.BreakerCooldownMax > 0 && c.Pool.BreakerCooldown > c.Pool.BreakerCooldownMax {
		return fmt.Errorf("pool.breaker_cooldown (%s) must not exceed pool.breaker_cooldown_max (%s)",
			c.Pool.BreakerCooldown, c.Pool.BreakerCooldownMax)
	}
	if c.Session.Capacity < 1 {
		return fmt.Errorf("session.capacity must be >= 1")
	}
	if c.Pool.ExpiringSoon < 0 {
		return fmt.Errorf("额度到期优先窗口不能为负数")
	}
	switch c.Prompts.Mode {
	case "passthrough", "replace":
	default:
		return fmt.Errorf("prompts.mode %q must be passthrough or replace", c.Prompts.Mode)
	}
	if c.Prompts.Mode == "replace" && strings.TrimSpace(c.Prompts.Text) == "" {
		return fmt.Errorf("prompts.mode is replace but prompts.text is empty")
	}
	if strings.TrimSpace(c.Upstream.ProxyURL) != "" {
		if _, err := url.Parse(c.Upstream.ProxyURL); err != nil {
			return fmt.Errorf("upstream.proxy_url %q is not a valid URL: %w", c.Upstream.ProxyURL, err)
		}
	}
	for _, h := range c.Schedule.CheckinHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule.checkin_hours contains %d, must be 0-23", h)
		}
	}
	for _, h := range c.Schedule.KeepaliveHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule.keepalive_hours contains %d, must be 0-23", h)
		}
	}
	if err := c.validateAccess(); err != nil {
		return err
	}
	return nil
}

// validateAccess enforces the exposure rules.
//
// The rules exist because this gateway lends out the operator's OWN accounts:
// an exposed instance without authentication is an open proxy onto their quota,
// and on the public internet that is discovered and abused within hours.
func (c *Config) validateAccess() error {
	if c.Access.Mode != "" && c.Access.Mode != AccessLoopback {
		return fmt.Errorf("自用版仅支持本机访问")
	}
	if strings.TrimSpace(c.Armor.Default) != "" && c.Armor.Dir == "" {
		return fmt.Errorf("armor.default requires armor.dir")
	}
	return nil
}

// ListenAddress resolves the configured access mode into a bind address.
//
// It is separate from Validate because resolving the LAN interface requires
// touching the network stack, which a validation pass should not do.
func (c *Config) ListenAddress() (string, error) {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return "", err
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	return c.Listen, nil
}

// inContainer reports whether this process is running inside a container.
//
// It is detected by the marker file the container runtime creates, not by an
// environment variable the operator could set by accident. The distinction
// matters because the loopback rule relaxes inside a container, and that
// relaxation must not be reachable on a bare host.
func inContainer() bool {
	for _, marker := range []string{
		"/.dockerenv",
		"/run/.containerenv",
	} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// IsLoopbackListen reports whether host:port binds only to the loopback
// interface. An empty host means all interfaces, which is NOT loopback.
func IsLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
