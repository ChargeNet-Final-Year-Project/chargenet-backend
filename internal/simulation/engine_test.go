package simulation

import (
	"math"
	"math/rand"
	"testing"
)

// The reference session used throughout: a 60 kWh car on a 150 kW DC connector,
// the same numbers the reference charging scenario uses.
const (
	testBattery = 60.0
	testCapKW   = 150.0
	testTariff  = 1.20
)

func TestPowerCurveSegments(t *testing.T) {
	// Plan 2.8, evaluated at the segment boundaries with cap = 150 kW:
	// 60 % at 0 %, 100 % at 10 %, 70 % at 80 % and 10 % at 100 %.
	cases := []struct {
		soc  float64
		want float64
	}{
		{0, 90}, {10, 150}, {80, 105}, {100, 15},
	}
	for _, c := range cases {
		if got := PowerKW(c.soc, testCapKW, nil); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("PowerKW(%.0f, 150, nil) = %v, want %v", c.soc, got, c.want)
		}
	}

	// The curve must join up at both seams, or a session would jump power the
	// moment it crossed 10 % or 80 %.
	for _, seam := range []float64{10, 80} {
		const delta = 1e-9
		below := PowerKW(seam-delta, testCapKW, nil)
		above := PowerKW(seam+delta, testCapKW, nil)
		if math.Abs(below-above) > 1e-6 {
			t.Errorf("curve is discontinuous at %.0f%%: %v vs %v", seam, below, above)
		}
	}

	// Clamping keeps the ramp-in nonnegative and prevents the taper rising again.
	if got := PowerKW(-20, testCapKW, nil); got != 90 {
		t.Errorf("PowerKW(-20, 150, nil) = %v, want the clamped 90", got)
	}
	if got := PowerKW(150, testCapKW, nil); math.Abs(got-15) > 1e-9 {
		t.Errorf("PowerKW(150, 150, nil) = %v, want the clamped 15", got)
	}
	if got := PowerKW(50, 0, nil); got != 0 {
		t.Errorf("PowerKW with no cap = %v, want 0", got)
	}
}

func TestPowerNeverExceedsCap(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, capKW := range []float64{testCapKW, VehicleAcceptKWAC, 22} {
		for soc := 0.0; soc <= 100; soc += 0.25 {
			got := PowerKW(soc, capKW, rng)
			if got < 0 || got > capKW {
				t.Fatalf("PowerKW(%.2f, %v, rng) = %v, want within [0, %v]", soc, capKW, got, capKW)
			}
		}
	}
}

func TestJitterStaysWithinOneAndAHalfPercent(t *testing.T) {
	// At 10 % SoC the ideal curve is exactly at the cap, so every jittered draw
	// would exceed it without the cap.
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 500; i++ {
		if got := PowerKW(10, testCapKW, rng); got > testCapKW {
			t.Fatalf("jittered power %v exceeded the cap %v", got, testCapKW)
		}
	}

	// Halfway down the ramp-in the jitter is visible but small: 5 % SoC sits at
	// 0.6 + 0.04×5 = 80 % of the cap, so ±1.5 % of 120 kW is ±1.8 kW.
	rng = rand.New(rand.NewSource(5))
	for i := 0; i < 500; i++ {
		got := PowerKW(5, testCapKW, rng)
		if math.Abs(got-120) > 1.8+1e-9 {
			t.Fatalf("PowerKW(5, 150, rng) = %v, want 120 ± 1.8", got)
		}
	}
}

func TestCapKW(t *testing.T) {
	cases := []struct {
		connector, accept, want float64
	}{
		{150, 150, 150}, // DC car on a DC charger
		{150, 11, 11},   // AC car plugged into a DC charger
		{22, 11, 11},    // AC car on an AC charger
		{22, 150, 22},   // DC car on an AC charger
		{22, 0, 22},     // no vehicle data: the connector is the only limit
		{150, 0, 150},
	}
	for _, c := range cases {
		if got := CapKW(c.connector, c.accept); got != c.want {
			t.Errorf("CapKW(%v, %v) = %v, want %v", c.connector, c.accept, got, c.want)
		}
	}
}

func TestVehicleAcceptKWFallbacks(t *testing.T) {
	cases := map[string]float64{"AC": 11, "ac": 11, "DC": 150, "": 150, "DC_HIGH": 150}
	for powerType, want := range cases {
		if got := VehicleAcceptKW(powerType); got != want {
			t.Errorf("VehicleAcceptKW(%q) = %v, want %v", powerType, got, want)
		}
	}
}

func TestStartSoCStaysInTheDocumentedBand(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	for i := 0; i < 2000; i++ {
		if got := StartSoC(rng); got < MinStartSoC || got > MaxStartSoC {
			t.Fatalf("StartSoC = %v, want within [%v, %v]", got, MinStartSoC, MaxStartSoC)
		}
	}
	if got := StartSoC(nil); got != (MinStartSoC+MaxStartSoC)/2 {
		t.Errorf("StartSoC(nil) = %v, want the midpoint %v", got, (MinStartSoC+MaxStartSoC)/2)
	}
}

// params builds DC parameters for the reference session.
func params(target float64) Params {
	return Params{
		BatteryKWh:      testBattery,
		ConnectorMaxKW:  testCapKW,
		VehicleAcceptKW: testCapKW,
		TariffPerKWh:    testTariff,
		TargetSoC:       target,
	}
}

func TestStepKeepsSoCEnergyAndCostConsistent(t *testing.T) {
	p := params(80)
	st := State{BatterySoC: 20}
	rng := rand.New(rand.NewSource(21))

	const dt = 10.0 // ten simulated seconds per tick
	for ticks := 0; ; ticks++ {
		if ticks > 10_000 {
			t.Fatal("session never reached its target SoC")
		}

		next, done, reason := Step(st, p, dt, rng)

		if next.BatterySoC < st.BatterySoC {
			t.Fatalf("SoC went backwards: %v → %v", st.BatterySoC, next.BatterySoC)
		}
		if next.KWhDelivered < st.KWhDelivered {
			t.Fatalf("energy went backwards: %v → %v", st.KWhDelivered, next.KWhDelivered)
		}
		if want := st.DurationSeconds + int(dt); next.DurationSeconds != want {
			t.Fatalf("duration = %d, want %d (simulated time)", next.DurationSeconds, want)
		}
		if want := Round2(math.Round(next.KWhDelivered*1000) / 1000 * testTariff); next.Cost != want {
			t.Fatalf("cost = %v, want round2(kWh × tariff) = %v", next.Cost, want)
		}
		if next.CurrentPowerKW > testCapKW {
			t.Fatalf("power %v exceeded the cap %v", next.CurrentPowerKW, testCapKW)
		}

		// Energy and SoC must tell the same story, tick after tick.
		energyFromSoC := (next.BatterySoC - 20) / 100 * testBattery
		if math.Abs(energyFromSoC-next.KWhDelivered) > 1e-9 {
			t.Fatalf("SoC says %v kWh delivered, the counter says %v", energyFromSoC, next.KWhDelivered)
		}

		st = next
		if done {
			if reason != ReasonTargetReached {
				t.Fatalf("reason = %q, want %q", reason, ReasonTargetReached)
			}
			break
		}
	}

	if st.BatterySoC != 80 {
		t.Fatalf("final SoC = %v, want exactly the 80 %% target", st.BatterySoC)
	}
	if math.Abs(st.KWhDelivered-testBattery*0.6) > 1e-9 {
		t.Fatalf("final energy = %v kWh, want %v", st.KWhDelivered, testBattery*0.6)
	}
}

// TestStepSessionDurationMatchesTheCurve pins the whole 20 % → 80 % DC session
// against the curve's closed form, calculated independently of the implementation.
//
// the reference charging scenario expects "between 25 and 40 simulated minutes" for
// this session, and that figure does not follow from the curve: power is linear
// in SoC over 10–80 %, so with a 150 kW cap it averages ~124 kW and 36 kWh takes
// ~17.5 minutes. The formula is the contract and the expected range is not, so
// this test asserts the curve and the card's implementation notes report the
// mismatch instead of quietly retuning the physics to hit a number.
func TestStepSessionDurationMatchesTheCurve(t *testing.T) {
	p := params(80)

	// Closed form straight from the charging model: on this range
	// P(s) = cap × (1 − 0.3(s−10)/70), so ∫ ds/P(s) is a logarithm. The integral
	// gives hours (energy in kWh over power in kW), hence the 3600.
	slope := testCapKW * 0.3 / 70
	p20 := testCapKW * (1.0 - 0.3*(20-10)/70)
	p80 := testCapKW * (1.0 - 0.3*(80-10)/70)
	wantSeconds := 3600 * (testBattery / 100) * (1 / slope) * math.Log(p20/p80)

	t.Logf("the curve implies %.1f simulated minutes for a 60 kWh / 150 kW "+
		"session from 20 %% to 80 %% (expected range: 25–40)",
		wantSeconds/60)

	// Ten simulated seconds per tick: the forward-Euler step is very close to
	// the integral, so 2 % is a generous tolerance.
	st := State{BatterySoC: 20}
	rng := rand.New(rand.NewSource(31))
	for {
		next, done, _ := Step(st, p, 10, rng)
		st = next
		if done {
			break
		}
	}

	got := float64(st.DurationSeconds)
	if math.Abs(got-wantSeconds)/wantSeconds > 0.02 {
		t.Fatalf("session took %.0f simulated seconds, the curve implies %.0f (±2 %%)", got, wantSeconds)
	}
}

func TestStepTrimsTheTickThatReachesTheTarget(t *testing.T) {
	// A whole simulated hour in one step would fly past 80 % (36 kWh needs only
	// ~15 minutes at this power); the session must stop exactly on the target,
	// with exactly the energy that implies.
	p := params(80)
	st := State{BatterySoC: 20}

	next, done, reason := Step(st, p, 3600, nil)
	if !done || reason != ReasonTargetReached {
		t.Fatalf("done = %v, reason = %q, want true / %q", done, reason, ReasonTargetReached)
	}
	if next.BatterySoC != 80 {
		t.Fatalf("SoC = %v, want the clamped 80 %%", next.BatterySoC)
	}
	if math.Abs(next.KWhDelivered-36) > 1e-9 {
		t.Fatalf("energy = %v kWh, want exactly the 36 kWh the last 60 %% needs", next.KWhDelivered)
	}
	if want := 43.20; next.Cost != want {
		t.Fatalf("cost = %v, want %v (36 kWh × 1.20)", next.Cost, want)
	}
	if next.DurationSeconds != 3600 {
		t.Fatalf("duration = %d, want 3600 (that time really passed)", next.DurationSeconds)
	}
}

func TestStepAlreadyAtTarget(t *testing.T) {
	st := State{BatterySoC: 80, KWhDelivered: 36, Cost: 43.20, DurationSeconds: 300}
	next, done, reason := Step(st, params(80), 10, nil)

	if !done || reason != ReasonTargetReached {
		t.Fatalf("done = %v, reason = %q, want true / %q", done, reason, ReasonTargetReached)
	}
	if next != st {
		t.Fatalf("state changed on a finished session: %+v → %+v", st, next)
	}
}

func TestStepWithoutTargetKeepsCharging(t *testing.T) {
	p := params(0) // no target: only an outside stop can end this session
	st := State{BatterySoC: 20}

	for i := 0; i < 50; i++ {
		next, done, _ := Step(st, p, 10, nil)
		st = next
		if done {
			t.Fatalf("session reported done after %d ticks without a target SoC", i+1)
		}
	}
	if st.BatterySoC <= 20 {
		t.Fatalf("SoC = %v, want progress without a target", st.BatterySoC)
	}
}

func TestStepRespectsAnACVehicleLimit(t *testing.T) {
	// A 22 kW AC connector with an 11 kW car: the vehicle, not the connector,
	// sets the ceiling all the way up the curve.
	p := Params{
		BatteryKWh:      testBattery,
		ConnectorMaxKW:  22,
		VehicleAcceptKW: 11,
		TariffPerKWh:    testTariff,
	}
	st := State{BatterySoC: 20}
	rng := rand.New(rand.NewSource(41))

	for i := 0; i < 200; i++ {
		next, _, _ := Step(st, p, 60, rng)
		st = next
		if st.CurrentPowerKW > VehicleAcceptKWAC {
			t.Fatalf("power %v kW exceeded the car's %v kW acceptance", st.CurrentPowerKW, VehicleAcceptKWAC)
		}
	}
}

func TestStepIgnoresUnusableInput(t *testing.T) {
	st := State{BatterySoC: 20, KWhDelivered: 1}
	for _, tc := range []struct {
		name string
		p    Params
		dt   float64
	}{
		{"no battery capacity", Params{ConnectorMaxKW: testCapKW, TargetSoC: 80}, 10},
		{"no time", Params{BatteryKWh: testBattery, TargetSoC: 80}, 0},
		{"negative time", Params{BatteryKWh: testBattery, TargetSoC: 80}, -10},
	} {
		next, done, _ := Step(st, tc.p, tc.dt, nil)
		if done || next != st {
			t.Errorf("%s: state advanced to %+v (done=%v), want it untouched", tc.name, next, done)
		}
	}
}

func TestRound2(t *testing.T) {
	cases := map[float64]float64{1.234: 1.23, 1.235: 1.24, 0: 0, 36 * 1.2: 43.2}
	for in, want := range cases {
		if got := Round2(in); got != want {
			t.Errorf("Round2(%v) = %v, want %v", in, got, want)
		}
	}
}
