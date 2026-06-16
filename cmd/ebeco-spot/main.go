// Command ebeco-spot controls an Ebeco EB-Therm thermostat from a spot-hinta.fi
// heating schedule: cheap periods raise the floor target to a baseline, the rest
// drop it to a low setpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/paveq/ebeco-spot/internal/baseline"
	"github.com/paveq/ebeco-spot/internal/config"
	"github.com/paveq/ebeco-spot/internal/control"
	"github.com/paveq/ebeco-spot/internal/ebeco"
	"github.com/paveq/ebeco-spot/internal/spothinta"
)

func main() {
	configPath := flag.String("config", envOr("EBECO_CONFIG", "config.toml"), "path to TOML config file")
	debug := flag.Bool("debug", false, "enable debug logging")
	list := flag.Bool("list", false, "authenticate, print all devices (id, name, program) and exit")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	runFn := run
	if *list {
		runFn = listDevices
	}
	if err := runFn(log, *configPath); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// listDevices authenticates and prints every device on the account so the user
// can discover the ids/program names to put in the config.
func listDevices(log *slog.Logger, configPath string) error {
	cfg, err := config.LoadForList(configPath)
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

func run(log *slog.Logger, configPath string) error {
	cfg, err := config.Load(configPath)
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

	ctrl := control.New(cfg, eb, spothinta.New(), store, log)
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
