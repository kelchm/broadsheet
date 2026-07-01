package paperboy

import (
	"testing"
	"time"
)

func TestRotationSlot_DeterministicAndAdvances(t *testing.T) {
	const n = 6
	interval := time.Hour
	base := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

	s0 := rotationSlot(base, interval, n)
	if rotationSlot(base, interval, n) != s0 {
		t.Error("rotationSlot is not deterministic for a fixed time")
	}
	// Stable within an interval (a safe read across a poll window).
	if rotationSlot(base.Add(interval/2), interval, n) != s0 {
		t.Error("slot changed within a single interval")
	}
	// Advances by one each interval.
	if got := rotationSlot(base.Add(interval), interval, n); got != (s0+1)%n {
		t.Errorf("after one interval slot = %d, want %d", got, (s0+1)%n)
	}
	// Wraps around the source count.
	if got := rotationSlot(base.Add(time.Duration(n)*interval), interval, n); got != s0 {
		t.Errorf("after n intervals slot = %d, want wrap to %d", got, s0)
	}
}

func TestRotationSlot_Guards(t *testing.T) {
	if rotationSlot(time.Now(), time.Hour, 0) != 0 {
		t.Error("n=0 should return 0, not panic")
	}
	if rotationSlot(time.Now(), 0, 3) < 0 {
		t.Error("zero interval should not produce a negative slot")
	}
}
