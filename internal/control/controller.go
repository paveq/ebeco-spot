// Package control ports the Shelly spot-hinta heating logic to the Ebeco
// thermostat: it polls a PlanAhead schedule, decides whether heating should be
// on, and writes the corresponding target temperature only when the state
// actually changes — turning "on" into a baseline setpoint and "off" into a low
// setpoint.
package control

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/paveq/ebeco-spot/internal/baseline"
	"github.com/paveq/ebeco-spot/internal/config"
	"github.com/paveq/ebeco-spot/internal/ebeco"
	"github.com/paveq/ebeco-spot/internal/spothinta"
)

// refetchMargin reproduces the Shelly behaviour of refetching the plan three
// hours before its furthest-future entry.
const refetchMargin = 3 * time.Hour

// statusLogEvery throttles the periodic "still running" log line.
const statusLogEvery = 2 * time.Minute

// Controller owns all mutable control state. It is single-goroutine: every
// method runs from Run's loop.
type Controller struct {
	cfg   config.Config
	ebeco *ebeco.Client
	spot  *spothinta.Client
	store *baseline.Store
	log   *slog.Logger

	instructions    []spothinta.Period // sorted descending by EpochMs
	instructionsExp time.Time          // when to refetch the plan
	needReload      bool

	haveState  bool         // whether currentOn is meaningful yet
	currentOn  bool         // last decided logical state (for logging)
	appliedOn  map[int]bool // last successfully applied physical state, per device
	nextChange time.Time    // when the schedule next flips (for logging)

	lastStatusLog time.Time
}

// New builds a Controller.
func New(cfg config.Config, eb *ebeco.Client, spot *spothinta.Client, store *baseline.Store, log *slog.Logger) *Controller {
	return &Controller{
		cfg:        cfg,
		ebeco:      eb,
		spot:       spot,
		store:      store,
		log:        log,
		needReload: true,
		appliedOn:  make(map[int]bool),
	}
}

// Run captures startup baselines, then drives the control loop until ctx is
// cancelled.
func (c *Controller) Run(ctx context.Context) error {
	c.initBaselines(ctx)

	ticker := time.NewTicker(c.cfg.PollInterval.Duration)
	defer ticker.Stop()

	c.tick(ctx) // act immediately rather than waiting a full interval
	for {
		select {
		case <-ctx.Done():
			c.log.Info("shutting down")
			return ctx.Err()
		case <-ticker.C:
			c.tick(ctx)
		}
	}
}

// initBaselines reads each device's current target on startup and seeds the
// baseline store per the rules: an in-range target is captured; otherwise an
// existing stored value is kept, falling back to the configured default.
func (c *Controller) initBaselines(ctx context.Context) {
	for _, id := range c.cfg.DeviceIDs {
		dev, err := c.ebeco.GetDevice(ctx, id)
		if err != nil {
			c.log.Warn("startup: could not read device target", "device", id, "err", err)
			continue
		}
		c.log.Info("startup device state",
			"device", id, "name", dev.DisplayName,
			"selectedProgram", dev.SelectedProgram, "programState", dev.ProgramState,
			"powerOn", dev.PowerOn,
			"target", dev.TemperatureSet, "floor", dev.TemperatureFloor, "room", dev.TemperatureRoom)
		c.captureBaseline(id, dev.TemperatureSet)
	}
}

// tick is one control cycle.
func (c *Controller) tick(ctx context.Context) {
	now := time.Now()
	if c.needReload || now.After(c.instructionsExp) {
		if !c.reload(ctx, now) {
			// Reload failed; backup logic already applied. Try again next tick.
			c.maybeLogStatus(now)
			return
		}
	}
	c.reconcile(ctx, now)
	c.maybeLogStatus(now)
}

// reload fetches and stores a fresh plan. It returns false on failure, in which
// case backup logic has already been applied.
func (c *Controller) reload(ctx context.Context, now time.Time) bool {
	periods, err := c.spot.PlanAhead(ctx, c.spotParams())
	if err != nil {
		c.log.Warn("fetching spot-hinta plan failed; applying backup logic", "err", err)
		c.needReload = true
		c.applyBackup(ctx, now)
		return false
	}
	if len(periods) == 0 {
		c.log.Warn("spot-hinta returned an empty plan; applying backup logic")
		c.needReload = true
		c.applyBackup(ctx, now)
		return false
	}

	// Sort descending so index 0 is the furthest-future period start.
	slices.SortFunc(periods, func(a, b spothinta.Period) int { return cmp.Compare(b.EpochMs, a.EpochMs) })
	c.instructions = periods
	c.needReload = false
	c.instructionsExp = time.UnixMilli(periods[0].EpochMs).Add(-refetchMargin)

	c.log.Info("loaded spot-hinta plan",
		"periods", len(periods),
		"refetch_after", c.instructionsExp.Format(time.RFC3339))
	return true
}

// reconcile decides the desired state for now and applies it.
func (c *Controller) reconcile(ctx context.Context, now time.Time) {
	on, ok := c.desiredState(now)
	if !ok {
		// Stale or no covering period: fall back and force a refetch next tick.
		c.needReload = true
		c.applyBackup(ctx, now)
		return
	}
	c.apply(ctx, on)
}

// desiredState finds the period covering now and returns the (inversion-applied)
// heating decision. ok is false when the plan is stale or does not cover now,
// signalling the caller to fall back to backup logic.
func (c *Controller) desiredState(now time.Time) (on bool, ok bool) {
	if len(c.instructions) == 0 {
		return false, false
	}
	nowMs := now.UnixMilli()

	// If even the furthest-future period start is in the past, the plan is
	// fully consumed and must be refetched.
	if c.instructions[0].EpochMs < nowMs {
		return false, false
	}

	// Instructions are descending; the first entry whose start is at or before
	// now is the current period. The entry before it (larger epoch) is the next
	// boundary.
	for i, p := range c.instructions {
		if p.EpochMs <= nowMs {
			if i > 0 {
				c.nextChange = time.UnixMilli(c.instructions[i-1].EpochMs)
			} else {
				c.nextChange = time.Time{}
			}
			return c.withInversion(p.Result), true
		}
	}

	// now is before every period in the plan.
	return false, false
}

// apply writes the target temperature to every device whose physical state
// needs to change. Before each change it reads the live target and captures any
// in-range value as the baseline, so manual adjustments are persisted.
func (c *Controller) apply(ctx context.Context, on bool) {
	pending := make([]int, 0, len(c.cfg.DeviceIDs))
	for _, id := range c.cfg.DeviceIDs {
		if prev, ok := c.appliedOn[id]; !ok || prev != on {
			pending = append(pending, id)
		}
	}
	// Record the logical decision even when nothing needs writing.
	c.haveState = true
	c.currentOn = on
	if len(pending) == 0 {
		return
	}

	for _, id := range pending {
		// Read-before-write: persist a manually set baseline if it is in range.
		if dev, err := c.ebeco.GetDevice(ctx, id); err != nil {
			c.log.Warn("could not read current target before control", "device", id, "err", err)
		} else {
			c.captureBaseline(id, dev.TemperatureSet)
		}

		target := c.cfg.OffTemperature
		if on {
			target = c.baselineFor(id)
		}

		in := ebeco.UpdateInput{ID: id, TemperatureSet: &target}
		if c.cfg.EnforceManualMode {
			powerOn := true
			program := c.cfg.ProgramName
			in.PowerOn = &powerOn
			in.SelectedProgram = &program
		}

		if err := c.ebeco.UpdateDevice(ctx, in); err != nil {
			// Leave appliedOn unchanged so this device is retried next tick.
			c.log.Error("failed to set device target", "device", id, "on", on, "target", target, "err", err)
			continue
		}
		c.appliedOn[id] = on
		c.log.Info("set device target", "device", id, "on", on, "target", target)
	}
}

// applyBackup drives heating from the backup-hours list when the schedule is
// unavailable, honouring the inversion setting just like normal operation.
func (c *Controller) applyBackup(ctx context.Context, now time.Time) {
	hour := now.Hour()
	on := slices.Contains(c.cfg.BackupHours, hour)
	c.log.Warn("backup mode", "hour", hour, "heating_on", c.withInversion(on))
	c.apply(ctx, c.withInversion(on))
}

// captureBaseline stores target as the device's baseline when it falls inside
// the persistence window; otherwise it ensures a default exists.
func (c *Controller) captureBaseline(id int, target float64) {
	if target >= c.cfg.BaselineMin && target <= c.cfg.BaselineMax {
		if v, ok := c.store.Get(id); !ok || v != target {
			if err := c.store.Set(id, target); err != nil {
				c.log.Warn("persisting baseline failed", "device", id, "err", err)
				return
			}
			c.log.Info("captured baseline", "device", id, "baseline", target)
		}
		return
	}
	if _, ok := c.store.Get(id); !ok {
		if err := c.store.Set(id, c.cfg.OnTemperature); err != nil {
			c.log.Warn("persisting default baseline failed", "device", id, "err", err)
			return
		}
		c.log.Info("no persisted baseline; using default", "device", id, "baseline", c.cfg.OnTemperature)
	}
}

func (c *Controller) baselineFor(id int) float64 {
	if v, ok := c.store.Get(id); ok {
		return v
	}
	return c.cfg.OnTemperature
}

func (c *Controller) withInversion(result bool) bool {
	if c.cfg.Inverted {
		return !result
	}
	return result
}

func (c *Controller) spotParams() spothinta.Params {
	return spothinta.Params{
		Region:             c.cfg.Region,
		PriorityHours:      c.cfg.NightHours,
		PriceModifier:      c.cfg.PriceDifference,
		RanksAllowed:       c.cfg.SelectedPricePeriods,
		RankDuration:       c.cfg.PricePeriodLength,
		PriceAlwaysAllowed: c.cfg.PriceAlwaysAllowed,
		MaxPrice:           c.cfg.MaximumPrice,
	}
}

func (c *Controller) maybeLogStatus(now time.Time) {
	if now.Sub(c.lastStatusLog) < statusLogEvery {
		return
	}
	c.lastStatusLog = now

	state := "unknown"
	if c.haveState {
		if c.currentOn {
			state = "on"
		} else {
			state = "off"
		}
	}
	next := "n/a"
	if !c.nextChange.IsZero() {
		next = c.nextChange.Format(time.RFC3339)
	}
	c.log.Info("running", "heating", state, "next_change", next)
}
