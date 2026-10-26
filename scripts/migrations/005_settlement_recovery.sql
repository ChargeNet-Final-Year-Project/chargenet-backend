ALTER TABLE charging_sessions ADD COLUMN settlement_requested_status varchar(16);
ALTER TABLE charging_sessions ADD COLUMN settlement_requested_reason varchar(32);
-- A terminal meter snapshot is persisted before settlement. Recovery can
-- finish that transaction instead of restarting an already stopped charger.
