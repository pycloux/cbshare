// Package config holds cpsync's runtime configuration: built-in defaults,
// an optional YAML file and command-line flags, applied in that order, so
// that a flag always wins over the file and the file over the defaults.
//
// To add a setting: add a field to Config (with its yaml tag), its default
// in Default, a check in Validate, a flag in Parse (and its case in the
// fs.Visit switch), a line in Describe, and document it in README.md,
// docs/configuration.md and cpsync.example.yaml.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Update modes for pushing chat changes to browsers.
const (
	ModeSSE     = "sse"
	ModePolling = "polling"
)

// MaxUsersLimit is the highest accepted value of max_users. cpsync is meant
// for a small group; the limit keeps the admin's user list manageable.
const MaxUsersLimit = 100

// Config is the effective configuration of a cpsync server.
type Config struct {
	// Network
	Port int    `yaml:"port"`
	Bind string `yaml:"bind"` // empty means: auto-detect the primary LAN IP

	// Storage
	Database string `yaml:"database"`

	// TLS (both empty means: self-signed certificate)
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`

	// Chat
	MaxMessages   int `yaml:"max_messages"`
	MaxFileSizeMB int `yaml:"max_file_size_mb"`

	// Users: every account counts, the admin included
	MaxUsers int `yaml:"max_users"`

	// Real-time updates
	Mode         string        `yaml:"mode"`
	PollInterval time.Duration `yaml:"poll_interval"`

	// Security
	MaxFailedAttempts int           `yaml:"max_failed_attempts"`
	LockoutDuration   time.Duration `yaml:"lockout_duration"`
	SessionTimeout    time.Duration `yaml:"session_timeout"`
	PINLength         int           `yaml:"pin_length"`
}

// Options are the command-line settings that are not part of the config file.
type Options struct {
	ConfigFile  string
	Password    string
	Key         string
	ShowVersion bool
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Port:              8443,
		Bind:              "",
		Database:          "cpsync.db",
		MaxMessages:       20,
		MaxFileSizeMB:     20,
		MaxUsers:          10,
		Mode:              ModeSSE,
		PollInterval:      3 * time.Second,
		MaxFailedAttempts: 5,
		LockoutDuration:   5 * time.Minute,
		SessionTimeout:    30 * time.Minute,
		PINLength:         6,
	}
}

// MaxFileSize returns the maximum upload size in bytes.
func (c Config) MaxFileSize() int64 { return int64(c.MaxFileSizeMB) << 20 }

// Validate checks that every value is usable.
func (c Config) Validate() error {
	var errs []string
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, "port must be between 1 and 65535")
	}
	if c.Database == "" {
		errs = append(errs, "database path must not be empty")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, "tls_cert and tls_key must be set together")
	}
	if c.MaxMessages < 1 {
		errs = append(errs, "max_messages must be at least 1")
	}
	if c.MaxFileSizeMB < 1 {
		errs = append(errs, "max_file_size_mb must be at least 1")
	}
	if c.MaxUsers < 1 || c.MaxUsers > MaxUsersLimit {
		errs = append(errs, fmt.Sprintf("max_users must be between 1 and %d", MaxUsersLimit))
	}
	if c.Mode != ModeSSE && c.Mode != ModePolling {
		errs = append(errs, `mode must be "sse" or "polling"`)
	}
	if c.PollInterval < 500*time.Millisecond {
		errs = append(errs, "poll_interval must be at least 500ms")
	}
	if c.MaxFailedAttempts < 1 {
		errs = append(errs, "max_failed_attempts must be at least 1")
	}
	if c.LockoutDuration <= 0 {
		errs = append(errs, "lockout_duration must be positive")
	}
	if c.SessionTimeout <= 0 {
		errs = append(errs, "session_timeout must be positive")
	}
	if c.PINLength < 4 || c.PINLength > 32 {
		errs = append(errs, "pin_length must be between 4 and 32")
	}
	if len(errs) > 0 {
		return errors.New("invalid configuration: " + strings.Join(errs, "; "))
	}
	return nil
}

// LoadFile overlays the YAML file at path onto c. Unknown keys are rejected
// so that typos do not silently fall back to defaults.
func (c *Config) LoadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	return nil
}

// Parse builds the effective configuration from defaults, the optional YAML
// file given with --config, and finally the flags explicitly set by the user.
func Parse(args []string, stderr io.Writer) (Config, Options, error) {
	cfg := Default()
	var opts Options

	fs := flag.NewFlagSet("cpsync", flag.ContinueOnError)
	fs.SetOutput(stderr)

	// The flag package writes every flag's value, set or not, so flags are
	// parsed into a separate Config (fl). Only the flags the user actually
	// typed are then copied over the file values, in the fs.Visit below.
	fl := Default()
	fs.StringVar(&opts.ConfigFile, "config", "", "path to a YAML configuration file")
	fs.StringVar(&opts.Password, "password", "", "admin password (first start only: creates the database and prints the key)")
	fs.StringVar(&opts.Key, "key", "", "database key printed at first start (used to restart the server)")
	fs.BoolVar(&opts.ShowVersion, "version", false, "print the version and exit")

	fs.IntVar(&fl.Port, "port", fl.Port, "HTTPS port")
	fs.StringVar(&fl.Bind, "bind", fl.Bind, "IP address to listen on (default: primary LAN IP; 0.0.0.0 for all)")
	fs.StringVar(&fl.Database, "db", fl.Database, "path to the encrypted database file")
	fs.StringVar(&fl.TLSCert, "tls-cert", fl.TLSCert, "PEM certificate file (default: self-signed)")
	fs.StringVar(&fl.TLSKey, "tls-key", fl.TLSKey, "PEM private key file for --tls-cert")
	fs.IntVar(&fl.MaxMessages, "max-messages", fl.MaxMessages, "number of messages kept in the chat")
	fs.IntVar(&fl.MaxFileSizeMB, "max-file-size", fl.MaxFileSizeMB, "maximum file size in MB")
	fs.IntVar(&fl.MaxUsers, "max-users", fl.MaxUsers, "maximum number of user accounts, the admin included")
	fs.StringVar(&fl.Mode, "mode", fl.Mode, `update mode: "sse" or "polling"`)
	fs.DurationVar(&fl.PollInterval, "poll-interval", fl.PollInterval, "browser polling interval in polling mode")
	fs.IntVar(&fl.MaxFailedAttempts, "max-attempts", fl.MaxFailedAttempts, "failed authentication attempts before an IP is locked out")
	fs.DurationVar(&fl.LockoutDuration, "lockout", fl.LockoutDuration, "lockout duration after too many failed attempts")
	fs.DurationVar(&fl.SessionTimeout, "session-timeout", fl.SessionTimeout, "session inactivity timeout")
	fs.IntVar(&fl.PINLength, "pin-length", fl.PINLength, "length of the setup PIN")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "cpsync - share text and files on your LAN\n\n")
		fmt.Fprintf(stderr, "First start:  cpsync --password <admin password> [options]\n")
		fmt.Fprintf(stderr, "Restart:      cpsync --key <key printed at first start> [options]\n\n")
		fmt.Fprintf(stderr, "Options:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return cfg, opts, err
	}
	if fs.NArg() > 0 {
		return cfg, opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if opts.ConfigFile != "" {
		if err := cfg.LoadFile(opts.ConfigFile); err != nil {
			return cfg, opts, err
		}
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Port = fl.Port
		case "bind":
			cfg.Bind = fl.Bind
		case "db":
			cfg.Database = fl.Database
		case "tls-cert":
			cfg.TLSCert = fl.TLSCert
		case "tls-key":
			cfg.TLSKey = fl.TLSKey
		case "max-messages":
			cfg.MaxMessages = fl.MaxMessages
		case "max-file-size":
			cfg.MaxFileSizeMB = fl.MaxFileSizeMB
		case "max-users":
			cfg.MaxUsers = fl.MaxUsers
		case "mode":
			cfg.Mode = fl.Mode
		case "poll-interval":
			cfg.PollInterval = fl.PollInterval
		case "max-attempts":
			cfg.MaxFailedAttempts = fl.MaxFailedAttempts
		case "lockout":
			cfg.LockoutDuration = fl.LockoutDuration
		case "session-timeout":
			cfg.SessionTimeout = fl.SessionTimeout
		case "pin-length":
			cfg.PINLength = fl.PINLength
		}
	})
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))

	return cfg, opts, cfg.Validate()
}

// Describe returns a human-readable summary of the effective configuration.
func (c Config) Describe() string {
	bind := c.Bind
	if bind == "" {
		bind = "auto (primary LAN IP)"
	}
	tls := "self-signed"
	if c.TLSCert != "" {
		tls = c.TLSCert
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  bind                %s\n", bind)
	fmt.Fprintf(&b, "  port                %d\n", c.Port)
	fmt.Fprintf(&b, "  database            %s\n", c.Database)
	fmt.Fprintf(&b, "  tls certificate     %s\n", tls)
	fmt.Fprintf(&b, "  max messages        %d\n", c.MaxMessages)
	fmt.Fprintf(&b, "  max file size       %d MB\n", c.MaxFileSizeMB)
	fmt.Fprintf(&b, "  max users           %d\n", c.MaxUsers)
	fmt.Fprintf(&b, "  update mode         %s\n", c.Mode)
	if c.Mode == ModePolling {
		fmt.Fprintf(&b, "  poll interval       %s\n", c.PollInterval)
	}
	fmt.Fprintf(&b, "  failed attempts     %d before a %s lockout\n", c.MaxFailedAttempts, c.LockoutDuration)
	fmt.Fprintf(&b, "  session timeout     %s\n", c.SessionTimeout)
	return b.String()
}
