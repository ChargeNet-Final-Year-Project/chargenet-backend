ALTER TABLE cpo_partners ADD COLUMN module_endpoints jsonb NOT NULL DEFAULT '{}';
-- Label the original shipped emulator network too. Operator-managed records
-- outside the stable bootstrap IDs/development credentials are not changed.
UPDATE stations s SET is_simulated=true FROM cpo_partners c
 WHERE c.id=s.cpo_id AND c.token_b=c.slug||'-token-b-dev'
 AND s.id IN ('stn_vlt_sgb','stn_vlt_gsr','stn_vlt_srr','stn_vlt_pjs',
 'stn_csy_mvl','stn_csy_pav','stn_csy_1u','stn_csy_swp',
 'stn_eco_klc','stn_eco_bsg','stn_eco_ttd','stn_eco_cyb','stn_eco_bbg');

CREATE FUNCTION protect_cdr_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.session_id,NEW.cpo_id,NEW.user_id,NEW.total_energy_kwh,NEW.total_time_seconds,
 NEW.gross_amount,NEW.platform_fee,NEW.net_payout,NEW.currency,NEW.cdr_json,NEW.ocpi_version)
 IS DISTINCT FROM ROW(OLD.session_id,OLD.cpo_id,OLD.user_id,OLD.total_energy_kwh,OLD.total_time_seconds,
 OLD.gross_amount,OLD.platform_fee,OLD.net_payout,OLD.currency,OLD.cdr_json,OLD.ocpi_version)
 THEN RAISE EXCEPTION 'settled CDRs are immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cdr_settlement_immutable BEFORE UPDATE ON cdrs
 FOR EACH ROW EXECUTE FUNCTION protect_cdr_settlement();
