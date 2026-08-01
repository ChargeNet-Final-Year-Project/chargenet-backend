package geo

import (
	"math"
	"testing"
)

func TestHaversineKnownDistance(t *testing.T) {
	// KLCC (3.1578,101.7123) → Mid Valley (3.1177,101.6770) ≈ 5.6 km.
	got := Round1(HaversineKM(3.1578, 101.7123, 3.1177, 101.6770))
	if got < 5.0 || got > 6.5 {
		t.Fatalf("distance KLCC→MidValley = %.1f km, want ~5–6 km", got)
	}
}

func TestHaversineSamePoint(t *testing.T) {
	if got := HaversineKM(3.14, 101.69, 3.14, 101.69); math.Abs(got) > 1e-9 {
		t.Fatalf("distance of a point to itself = %v, want 0", got)
	}
}

func TestRound1(t *testing.T) {
	cases := map[float64]float64{1.24: 1.2, 1.25: 1.3, 1.96: 2.0, 0.0: 0.0}
	for in, want := range cases {
		if got := Round1(in); got != want {
			t.Errorf("Round1(%v) = %v, want %v", in, got, want)
		}
	}
}
