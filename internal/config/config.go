// Package config loads runtime configuration from a TOML file plus
// credentials from the environment, applies sensible defaults, and validates.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration that decodes from a TOML string such as "15s".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler so BurntSushi/toml can
// decode duration strings like "15s" or "2m".
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Config is the full runtime configuration. Everything except credentials is
// read from the TOML file; Email/Password come from the environment.
type Config struct {
	// Credentials — populated from EBECO_EMAIL / EBECO_PASSWORD, never the file.
	Email    string `toml:"-"`
	Password string `toml:"-"`

	// Devices to control, addressed by their Ebeco device id.
	DeviceIDs []int `toml:"device_ids"`

	// spot-hinta PlanAhead parameters — direct mirror of the Shelly script.
	Region               string   `toml:"region"`                 // e.g. "FI"
	SelectedPricePeriods []string `toml:"selected_price_periods"` // ranksAllowed, e.g. ["1-10"]
	PricePeriodLength    int      `toml:"price_period_length"`    // rankDuration, minutes
	NightHours           []int    `toml:"night_hours"`            // priorityHours
	PriceDifference      int      `toml:"price_difference"`       // priceModifier
	OnlyNightHours       bool     `toml:"only_night_hours"`       // forces priceModifier = -999
	PriceAlwaysAllowed   int      `toml:"price_always_allowed"`   // -99 disables
	MaximumPrice         int      `toml:"maximum_price"`          // maxPrice, euro cents
	BackupHours          []int    `toml:"backup_hours"`           // heat ON during these hours if spot-hinta is unreachable
	Inverted             bool     `toml:"inverted"`               // invert on/off logic

	// Temperature control, degrees Celsius.
	OnTemperature  float64 `toml:"on_temperature"`  // default baseline when none is persisted
	OffTemperature float64 `toml:"off_temperature"` // setpoint applied when "off"
	BaselineMin    float64 `toml:"baseline_min"`    // persist current target as baseline only within
	BaselineMax    float64 `toml:"baseline_max"`    //   the inclusive [min,max] window

	// Heating-mode enforcement: on every write also force powerOn + a constant
	// setpoint program so the spot-price setpoint is honoured.
	EnforceManualMode bool   `toml:"enforce_manual_mode"`
	ProgramName       string `toml:"program_name"` // selectedProgram value, e.g. "Manual"

	// Files, timing and endpoint overrides.
	StateFile    string   `toml:"state_file"`    // where per-device baselines are persisted
	PollInterval Duration `toml:"poll_interval"` // control loop tick, e.g. "15s"
	APIBaseURL   string   `toml:"api_base_url"`  // Ebeco Connect base URL
	APITenantID  int      `toml:"api_tenant_id"` // ABP Abp.TenantId header; Ebeco uses the default tenant (1)

	// Logging.
	LogOutput string `toml:"log_output"` // "stdout" (structured text) or "oslog" (macOS unified logging)
}

// LogOutput values.
const (
	LogStdout = "stdout"
	LogOSLog  = "oslog"
)

func defaults() Config {
	return Config{
		Region:               "FI",
		SelectedPricePeriods: []string{"1-10"},
		PricePeriodLength:    30,
		NightHours:           []int{22, 23, 0, 1, 2, 3, 4, 5, 6},
		PriceDifference:      0,
		OnlyNightHours:       false,
		PriceAlwaysAllowed:   -99,
		MaximumPrice:         20,
		BackupHours:          []int{3, 4, 5, 6},
		Inverted:             false,
		OnTemperature:        26,
		OffTemperature:       15,
		BaselineMin:          20,
		BaselineMax:          30,
		EnforceManualMode:    true,
		ProgramName:          "Manual",
		LogOutput:            LogStdout,
		StateFile:            "baseline.json",
		PollInterval:         Duration{15 * time.Second},
		APIBaseURL:           "https://ebecoconnect.com",
		APITenantID:          1,
	}
}

// Load reads the config file at path, overlays environment credentials, applies
// derived rules and fully validates the result for daemon operation.
func Load(path string) (Config, error) {
	cfg, err := read(path, false)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadForList is like Load but only requires credentials, not device_ids, and
// tolerates a missing config file. It backs the device-listing bootstrap mode.
func LoadForList(path string) (Config, error) {
	cfg, err := read(path, true)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.validateCreds(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// read loads defaults, overlays the file (optional when tolerateMissing) and
// environment credentials, and applies derived rules.
func read(path string, tolerateMissing bool) (Config, error) {
	cfg := defaults()
	if _, statErr := os.Stat(path); statErr != nil {
		if !(tolerateMissing && os.IsNotExist(statErr)) {
			return Config{}, fmt.Errorf("reading config %q: %w", path, statErr)
		}
		// No file: proceed with defaults + environment.
	} else if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("reading config %q: %w", path, err)
	}

	cfg.Email = os.Getenv("EBECO_EMAIL")
	cfg.Password = os.Getenv("EBECO_PASSWORD")

	switch cfg.LogOutput = strings.ToLower(strings.TrimSpace(cfg.LogOutput)); cfg.LogOutput {
	case "":
		cfg.LogOutput = LogStdout
	case LogStdout, LogOSLog:
	default:
		return Config{}, fmt.Errorf("log_output %q is invalid (want %q or %q)", cfg.LogOutput, LogStdout, LogOSLog)
	}

	// Mirror the Shelly script: searching only night hours is expressed by a
	// very negative price modifier.
	if cfg.OnlyNightHours {
		cfg.PriceDifference = -999
	}
	return cfg, nil
}

func (c Config) validateCreds() error {
	if c.Email == "" || c.Password == "" {
		return fmt.Errorf("EBECO_EMAIL and EBECO_PASSWORD must be set in the environment")
	}
	return nil
}

func (c Config) validate() error {
	if err := c.validateCreds(); err != nil {
		return err
	}
	if len(c.DeviceIDs) == 0 {
		return fmt.Errorf("device_ids must list at least one device id")
	}
	if c.PollInterval.Duration <= 0 {
		return fmt.Errorf("poll_interval must be positive")
	}
	if c.BaselineMin > c.BaselineMax {
		return fmt.Errorf("baseline_min (%.1f) must be <= baseline_max (%.1f)", c.BaselineMin, c.BaselineMax)
	}
	if c.OffTemperature >= c.BaselineMin {
		return fmt.Errorf("off_temperature (%.1f) should be below baseline_min (%.1f), otherwise turning off would be saved as a baseline", c.OffTemperature, c.BaselineMin)
	}
	if c.EnforceManualMode && c.ProgramName == "" {
		return fmt.Errorf("program_name must be set when enforce_manual_mode is true")
	}
	return nil
}
