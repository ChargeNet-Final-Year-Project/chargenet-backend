package simulation

import (
	"github.com/chargenet/chargenet-backend/internal/money"
	"math"
	"math/rand"
	"strconv"
)

// Session stop reasons. The gateway reports TARGET_REACHED, USER and FAULT;
// the session orchestrator adds
// BALANCE_EXHAUSTED and TIMEOUT because only it can see the wallet and the
// elapsed simulated time.
const (
	ReasonTargetReached    = "TARGET_REACHED"
	ReasonBalanceExhausted = "BALANCE_EXHAUSTED"
	ReasonTimeout          = "TIMEOUT"
	ReasonUser             = "USER"
	ReasonFault            = "FAULT"
	// ReasonRemoteStopped is the CPO deciding the charge is over:
	// the value an OCPI session ends with when the CPO reports it finished and
	// the driver did not ask for it.
	ReasonRemoteStopped = "REMOTE_STOPPED"
)

// Params contains the inputs for one charging session.
type Params struct {
	MaxEnergyKWh float64 `json:"maxEnergyKwh,omitempty"`
	// BatteryKWh is the pack size the SoC percentage is measured against.
	BatteryKWh float64 `json:"batteryKwh"`
	// ConnectorMaxKW is the connector's rated power.
	ConnectorMaxKW float64 `json:"connectorMaxKw"`
	// VehicleAcceptKW is what the car will take; <= 0 means no vehicle data.
	VehicleAcceptKW float64 `json:"vehicleAcceptKw"`
	// TariffPerKWh is the price of one delivered kWh, in MYR.
	TariffPerKWh float64 `json:"tariffPerKwh"`
	// TargetSoC is the state of charge the session stops at; <= 0 runs until
	// something else stops it.
	TargetSoC float64 `json:"targetSoc"`
}

// State is the live snapshot of one charging session: the numbers the mobile
// monitor renders and the columns the orchestrator persists on its
// throttled tick. The JSON names match the mobile `ChargingSession` DTO so
// telemetry maps across without a translation table.
type State struct {
	BatterySoC      float64 `json:"batterySoc"`
	KWhDelivered    float64 `json:"kwhDelivered"`
	DurationSeconds int     `json:"durationSeconds"`
	CurrentPowerKW  float64 `json:"currentPowerKw"`
	Cost            float64 `json:"cost"`
}

// Step advances st by simDtSec of simulated time, using the power curve for the
// current SoC, and reports whether the session has finished and why.
//
// Charging stops only for TARGET_REACHED here. Everything else (the driver's
// stop, an exhausted wallet, the 4-hour timeout) is decided outside this
// function, because only the caller can see the wallet or the wall clock.
//
// The tick that would cross the target SoC is trimmed to the target, so the
// reported SoC never overshoots what the driver asked for and the delivered
// energy stays consistent with it.
func Step(st State, p Params, simDtSec float64, rng *rand.Rand) (State, bool, string) {
	if p.BatteryKWh <= 0 || simDtSec <= 0 {
		return st, false, ""
	}
	if p.TargetSoC > 0 && st.BatterySoC >= p.TargetSoC {
		return st, true, ReasonTargetReached
	}

	kw := PowerKW(st.BatterySoC, CapKW(p.ConnectorMaxKW, p.VehicleAcceptKW), rng)

	kwhTick := kw * simDtSec / 3600
	soc := st.BatterySoC + kwhTick/p.BatteryKWh*100

	done := false
	if p.TargetSoC > 0 && soc >= p.TargetSoC {
		soc = p.TargetSoC
		kwhTick = (p.TargetSoC - st.BatterySoC) / 100 * p.BatteryKWh
		done = true
	}

	kwh := st.KWhDelivered + kwhTick
	budgetReached := p.MaxEnergyKWh > 0 && kwh >= p.MaxEnergyKWh
	if budgetReached {
		kwhTick = math.Max(0, p.MaxEnergyKWh-st.KWhDelivered)
		kwh = st.KWhDelivered + kwhTick
		soc = st.BatterySoC + kwhTick/p.BatteryKWh*100
	}
	original, _, _ := money.Charge(kwh, strconv.FormatFloat(p.TariffPerKWh, 'f', 6, 64), "1")
	next := State{
		BatterySoC:      soc,
		KWhDelivered:    kwh,
		DurationSeconds: st.DurationSeconds + int(math.Round(simDtSec)),
		CurrentPowerKW:  kw,
		Cost:            money.Float(original),
	}

	if budgetReached {
		return next, true, ReasonBalanceExhausted
	}
	if done {
		return next, true, ReasonTargetReached
	}
	return next, false, ""
}
