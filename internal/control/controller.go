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

// fetchBackoffMax caps the exponential backoff between spot-hinta plan fetches
// while the plan is unavailable or unusable. The base interval is the configured
// poll interval and doubles each failure up to this ceiling.
const fetchBackoffMax = time.Hour

// shutdownTimeout bounds the baseline-restore writes on clean shutdown. It is
// kept short so we finish before launchd escalates SIGTERM to SIGKILL.
const shutdownTimeout = 5 * time.Second

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

	// Exponential backoff for spot-hinta fetches: nextFetchAt gates the next
	// attempt; fetchBackoff is the current (doubling) gap, capped at
	// fetchBackoffMax. Reset whenever a fetch yields a usable plan.
	fetchBackoff time.Duration
	nextFetchAt  time.Time
	inBackup     bool // currently in backup mode (drives entering/leaving logs)

	haveState  bool         // whether currentOn is meaningful yet
	currentOn  bool         // last decided logical state (for logging)
	appliedOn  map[int]bool // last successfully applied physical state, per device
	nextChange time.Time    // when the schedule next flips (for logging)
}

// New builds a Controller.
func New(cfg config.Config, eb *ebeco.Client, spot *spothinta.Client, store *baseline.Store, log *slog.Logger) *Controller {
	return &Controller{
		cfg:          cfg,
		ebeco:        eb,
		spot:         spot,
		store:        store,
		log:          log,
		needReload:   true,
		appliedOn:    make(map[int]bool),
		fetchBackoff: cfg.PollInterval.Duration,
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
			c.restoreBaselines()
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
		c.logDeviceState("startup device state", id, dev)
		c.captureBaseline(id, dev.TemperatureSet)
	}
}

// tick is one control cycle: optionally refetch the plan (rate-limited by the
// fetch backoff), then reconcile the desired state. reconcile always runs so a
// still-valid older plan keeps driving heating even when a refetch is failing.
func (c *Controller) tick(ctx context.Context) {
	now := time.Now()
	if (c.needReload || now.After(c.instructionsExp)) && !now.Before(c.nextFetchAt) {
		c.reload(ctx, now)
	}
	c.reconcile(ctx, now)
	c.logStatus()
}

// reload fetches and stores a fresh plan. A failed, empty, or non-covering fetch
// grows the exponential backoff so we don't hammer spot-hinta; it does not apply
// backup itself — reconcile does that from whatever plan we end up with.
func (c *Controller) reload(ctx context.Context, now time.Time) {
	periods, err := c.spot.PlanAhead(ctx, c.spotParams())
	if err != nil {
		c.failFetch(now)
		c.log.Warn("fetching spot-hinta plan failed; backing off",
			"err", err, "retry_after", c.nextFetchAt.Format(time.RFC3339))
		return
	}
	if len(periods) == 0 {
		c.failFetch(now)
		c.log.Warn("spot-hinta returned an empty plan; backing off",
			"retry_after", c.nextFetchAt.Format(time.RFC3339))
		return
	}

	// Sort descending so index 0 is the furthest-future period start.
	slices.SortFunc(periods, func(a, b spothinta.Period) int { return cmp.Compare(b.EpochMs, a.EpochMs) })
	c.instructions = periods
	c.instructionsExp = time.UnixMilli(periods[0].EpochMs).Add(-refetchMargin)

	if _, ok := c.desiredState(now); !ok {
		// A plan that does not cover now (future-only or already stale) won't be
		// fixed by an immediate refetch, so back off like a failure. Matches the
		// Shelly script, which holds rather than refetch-storming in this case.
		c.failFetch(now)
		c.log.Warn("spot-hinta plan does not cover the current time; backing off",
			"periods", len(periods), "retry_after", c.nextFetchAt.Format(time.RFC3339))
		return
	}

	c.succeedFetch()
	c.log.Info("loaded spot-hinta plan",
		"periods", len(periods),
		"refetch_after", c.instructionsExp.Format(time.RFC3339))
}

// failFetch records a failed or unusable plan fetch: it forces a refetch and
// pushes the next attempt out by the current backoff, then doubles the backoff
// up to fetchBackoffMax.
func (c *Controller) failFetch(now time.Time) {
	c.needReload = true
	c.nextFetchAt = now.Add(c.fetchBackoff)
	c.fetchBackoff = min(c.fetchBackoff*2, fetchBackoffMax)
}

// succeedFetch records a good, covering plan: clear the refetch flag and reset
// the backoff to the base poll interval.
func (c *Controller) succeedFetch() {
	c.needReload = false
	c.fetchBackoff = c.cfg.PollInterval.Duration
	c.nextFetchAt = time.Time{}
}

// reconcile decides the desired state for now and applies it, falling back to
// backup logic when no plan covers now.
func (c *Controller) reconcile(ctx context.Context, now time.Time) {
	on, ok := c.desiredState(now)
	if !ok {
		// Stale or no covering period: force a refetch (rate-limited by the
		// backoff) and drive heating from backup logic until a plan returns.
		c.needReload = true
		c.applyBackup(ctx, now)
		return
	}
	if c.inBackup {
		c.log.Info("recovered from backup mode; spot-hinta plan now drives heating")
		c.inBackup = false
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
			c.logDeviceState("device state before control", id, dev)
			c.captureBaseline(id, dev.TemperatureSet)
		}

		target := c.cfg.OffTemperature
		if on {
			target = c.baselineFor(id)
		}

		if err := c.writeTarget(ctx, id, target); err != nil {
			// Leave appliedOn unchanged so this device is retried next tick.
			c.log.Error("failed to set device target", "device", id, "on", on, "target", target, "err", err)
			continue
		}
		c.appliedOn[id] = on
		c.log.Info("set device target", "device", id, "on", on, "target", target, "next_change", c.nextChangeStr())
	}
}

// restoreBaselines writes each device's baseline target on clean shutdown, so
// the thermostat is left at a comfortable setpoint rather than stuck at the
// "off" value. The controller's context is already cancelled by now, so it uses
// a fresh, short-lived context for the writes.
func (c *Controller) restoreBaselines() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, id := range c.cfg.DeviceIDs {
		target := c.baselineFor(id)
		if err := c.writeTarget(ctx, id, target); err != nil {
			c.log.Error("failed to restore baseline target on shutdown", "device", id, "target", target, "err", err)
			continue
		}
		c.log.Info("restored baseline target on shutdown", "device", id, "target", target)
	}
}

// writeTarget sets a device's target temperature, also forcing powerOn and the
// constant-setpoint program when manual-mode enforcement is enabled.
func (c *Controller) writeTarget(ctx context.Context, id int, target float64) error {
	in := ebeco.UpdateInput{ID: id, TemperatureSet: &target}
	if c.cfg.EnforceManualMode {
		powerOn := true
		program := c.cfg.ProgramName
		in.PowerOn = &powerOn
		in.SelectedProgram = &program
	}
	return c.ebeco.UpdateDevice(ctx, in)
}

// applyBackup drives heating from the backup-hours list when the schedule is
// unavailable, honouring the inversion setting just like normal operation.
func (c *Controller) applyBackup(ctx context.Context, now time.Time) {
	hour := now.Hour()
	on := c.withInversion(slices.Contains(c.cfg.BackupHours, hour))
	if !c.inBackup {
		c.log.Warn("entering backup mode; spot-hinta plan unavailable", "hour", hour, "heating_on", on)
		c.inBackup = true
	}
	c.nextChange = time.Time{} // no scheduled change is known while in backup
	// Per-tick detail at debug; the entering/leaving transitions above and the
	// "set device target" line in apply carry the signal at info/warn.
	c.log.Debug("backup mode", "hour", hour, "heating_on", on)
	c.apply(ctx, on)
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

func (c *Controller) logStatus() {
	state := "unknown"
	if c.haveState {
		if c.currentOn {
			state = "on"
		} else {
			state = "off"
		}
	}
	c.log.Debug("running", "heating", state, "next_change", c.nextChangeStr())
}

// logDeviceState emits a uniform device snapshot whenever we read a device, so
// the startup read and every read-before-write log the same fields.
func (c *Controller) logDeviceState(msg string, id int, dev ebeco.Device) {
	c.log.Info(msg,
		"device", id, "name", dev.DisplayName,
		"selectedProgram", dev.SelectedProgram, "programState", dev.ProgramState,
		"powerOn", dev.PowerOn, "relayOn", dev.RelayOn,
		"target", dev.TemperatureSet, "floor", dev.TemperatureFloor, "room", dev.TemperatureRoom)
}

// nextChangeStr renders the next scheduled flip for logging, or "n/a" when none
// is known (e.g. while in backup mode).
func (c *Controller) nextChangeStr() string {
	if c.nextChange.IsZero() {
		return "n/a"
	}
	return c.nextChange.Format(time.RFC3339)
}
