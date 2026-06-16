package control

import (
	"testing"
	"time"

	"github.com/paveq/ebeco-spot/internal/config"
	"github.com/paveq/ebeco-spot/internal/spothinta"
)

// plan is the example PlanAhead payload, already sorted descending as reload()
// would leave it.
var plan = []spothinta.Period{
	{EpochMs: 1781640000000, Result: true},
	{EpochMs: 1781636400000, Result: true},
	{EpochMs: 1781629200000, Result: true},
	{EpochMs: 1781623800000, Result: false},
	{EpochMs: 1781620200000, Result: true},
	{EpochMs: 1781589600000, Result: false},
}

func newTestController(inverted bool, periods []spothinta.Period) *Controller {
	return &Controller{
		cfg:          config.Config{Inverted: inverted},
		instructions: periods,
	}
}

func TestDesiredState(t *testing.T) {
	tests := []struct {
		name     string
		nowMs    int64
		inverted bool
		wantOn   bool
		wantOK   bool
		wantNext int64 // 0 means "no next change expected"
	}{
		{name: "covers middle period", nowMs: 1781630000000, wantOn: true, wantOK: true, wantNext: 1781636400000},
		{name: "exact boundary uses that period", nowMs: 1781623800000, wantOn: false, wantOK: true, wantNext: 1781629200000},
		{name: "earliest period, no later next when newest", nowMs: 1781589600000, wantOn: false, wantOK: true, wantNext: 1781620200000},
		{name: "stale: now past furthest entry", nowMs: 1781650000000, wantOK: false},
		{name: "now before whole plan", nowMs: 1781500000000, wantOK: false},
		{name: "inverted flips the result", nowMs: 1781630000000, inverted: true, wantOn: false, wantOK: true, wantNext: 1781636400000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestController(tc.inverted, plan)
			on, ok := c.desiredState(time.UnixMilli(tc.nowMs))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if on != tc.wantOn {
				t.Errorf("on = %v, want %v", on, tc.wantOn)
			}
			if tc.wantNext != 0 {
				if got := c.nextChange.UnixMilli(); got != tc.wantNext {
					t.Errorf("nextChange = %d, want %d", got, tc.wantNext)
				}
			}
		})
	}
}

func TestDesiredStateEmpty(t *testing.T) {
	c := newTestController(false, nil)
	if _, ok := c.desiredState(time.UnixMilli(1781630000000)); ok {
		t.Fatal("expected ok=false for empty plan")
	}
}
