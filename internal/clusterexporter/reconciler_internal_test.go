package clusterexporter

import (
	"testing"
	"time"
)

func TestAddJitter(t *testing.T) {
	const d = time.Hour
	for range 1000 {
		got := addJitter(d)
		if got < d {
			t.Fatalf("addJitter(%v) = %v, want >= d (jitter must extend, not shorten)", d, got)
		}
		if got > d+d/6 {
			t.Fatalf("addJitter(%v) = %v, want <= d+d/6 (%v)", d, got, d+d/6)
		}
	}
}

func TestAddJitter_ZeroOrNegative(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := addJitter(d); got != d {
			t.Errorf("addJitter(%v) = %v, want %v", d, got, d)
		}
	}
}
