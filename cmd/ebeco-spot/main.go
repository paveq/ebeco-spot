// Command ebeco-spot controls an Ebeco EB-Therm thermostat from a spot-hinta.fi
// heating schedule: cheap periods raise the floor target to a baseline, the rest
// drop it to a low setpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/paveq/ebeco-spot/internal/applog"
	"github.com/paveq/ebeco-spot/internal/baseline"
	"github.com/paveq/ebeco-spot/internal/config"
	"github.com/paveq/ebeco-spot/internal/control"
	"github.com/paveq/ebeco-spot/internal/ebeco"
	"github.com/paveq/ebeco-spot/internal/secrets"
	"github.com/paveq/ebeco-spot/internal/spothinta"
)

// Keychain item service names used with -keychain (macOS only).
const (
	keychainServiceEmail    = "ebeco-spot-email"
	keychainServicePassword = "ebeco-spot-password"
)

// options holds the parsed command-line flags.
type options struct {
	configPath string
	debug      bool
	logOutput  string
	keychain   bool
}

func main() {
	var opts options
	flag.StringVar(&opts.configPath, "config", envOr("EBECO_CONFIG", "config.toml"), "path to TOML config file")
	flag.BoolVar(&opts.debug, "debug", false, "enable debug logging")
	list := flag.Bool("list", false, "authenticate, print all devices (id, name, program) and exit")
	flag.StringVar(&opts.logOutput, "log", "", `log output: "stdout" or "oslog" (overrides log_output in config)`)
	flag.BoolVar(&opts.keychain, "keychain", false, "read EBECO_EMAIL/EBECO_PASSWORD from the macOS Keychain ("+keychainServiceEmail+"/"+keychainServicePassword+")")
	flag.Parse()

	runFn := run
	if *list {
		runFn = listDevices
	}
	if err := runFn(opts); err != nil {
		// Bootstrap logger: a configured logger may not exist yet on failure.
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("fatal", "err", err)
		os.Exit(1)
	}
}

// loadCredentials overlays EBECO_EMAIL/EBECO_PASSWORD from the Keychain when
// -keychain is set, so the rest of config loading is unchanged. Without the
// flag, credentials come from the environment as before.
func loadCredentials(opts options) error {
	if !opts.keychain {
		return nil
	}
	email, err := secrets.Read(keychainServiceEmail)
	if err != nil {
		return fmt.Errorf("reading %s: %w", keychainServiceEmail, err)
	}
	password, err := secrets.Read(keychainServicePassword)
	if err != nil {
		return fmt.Errorf("reading %s: %w", keychainServicePassword, err)
	}
	os.Setenv("EBECO_EMAIL", email)
	os.Setenv("EBECO_PASSWORD", password)
	return nil
}

// newLogger builds the application logger from the config, with -debug raising
// the level and a non-empty -log flag overriding the configured output.
func newLogger(cfg config.Config, opts options) (*slog.Logger, error) {
	level := slog.LevelInfo
	if opts.debug {
		level = slog.LevelDebug
	}
	output := cfg.LogOutput
	if opts.logOutput != "" {
		output = opts.logOutput
	}
	return applog.New(output, level)
}

// listDevices authenticates and prints every device on the account so the user
// can discover the ids/program names to put in the config.
func listDevices(opts options) error {
	if err := loadCredentials(opts); err != nil {
		return err
	}
	cfg, err := config.LoadForList(opts.configPath)
	if err != nil {
		return err
	}
	log, err := newLogger(cfg, opts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eb := ebeco.New(cfg.APIBaseURL, cfg.Email, cfg.Password, cfg.APITenantID, log)
	if err := eb.Authenticate(ctx); err != nil {
		return err
	}
	devs, err := eb.GetDevices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devs {
		log.Info("device",
			"id", d.ID, "name", d.DisplayName,
			"selectedProgram", d.SelectedProgram, "programState", d.ProgramState,
			"powerOn", d.PowerOn,
			"temperatureSet", d.TemperatureSet,
			"temperatureFloor", d.TemperatureFloor, "temperatureRoom", d.TemperatureRoom,
			"relayOn", d.RelayOn)
	}
	log.Info("device count", "n", len(devs))
	return nil
}

func run(opts options) error {
	if err := loadCredentials(opts); err != nil {
		return err
	}
	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}
	log, err := newLogger(cfg, opts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eb := ebeco.New(cfg.APIBaseURL, cfg.Email, cfg.Password, cfg.APITenantID, log)
	if err := eb.Authenticate(ctx); err != nil {
		return err
	}
	log.Info("authenticated with ebeco", "devices", cfg.DeviceIDs)

	store, err := baseline.Load(cfg.StateFile)
	if err != nil {
		return err
	}

	ctrl := control.New(cfg, eb, spothinta.New(log), store, log)
	if err := ctrl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("stopped")
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
