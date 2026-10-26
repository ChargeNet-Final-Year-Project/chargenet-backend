package store

import "context"

// RequestSettlement durably retains the first terminal meter snapshot. It is
// deliberately separate from wallet settlement so a rolled-back financial
// transaction still leaves enough information for retry/restart recovery.
func (s *Store) RequestSettlement(ctx context.Context, id, status, reason string, t SessionTelemetry) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE charging_sessions SET
 duration_seconds=$2,kwh_delivered=$3,current_power_kw=$4,battery_soc=$5,
 original_cost=round(round($3::numeric,3)*tariff_per_kwh,2),
 total_cost=round(round(round($3::numeric,3)*tariff_per_kwh,2)*exchange_rate,2),
 settlement_requested_status=$6,settlement_requested_reason=$7
 WHERE id=$1 AND status IN ('PENDING','CHARGING') AND settlement_requested_status IS NULL`,
		id, t.DurationSeconds, t.KWhDelivered, t.CurrentPowerKW, t.BatterySoC, status, reason)
	return err
}
