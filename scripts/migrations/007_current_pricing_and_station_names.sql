-- Keep historical expiry values for audit; new estimates have no deadline.
ALTER TABLE charging_quotes ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE charging_quotes ADD COLUMN rate_selection_source text NOT NULL DEFAULT '';

-- Acceptance may advance FX metadata only when the effective financial terms
-- are identical. Once accepted, the entire snapshot remains immutable.
CREATE OR REPLACE FUNCTION protect_quote_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'charging quotation snapshots are immutable'; END IF;
 IF OLD.accepted_at IS NOT NULL OR NEW.accepted_at IS NULL OR
   (to_jsonb(NEW)-ARRAY['accepted_at','exchange_rate_id','rate_source']) IS DISTINCT FROM
   (to_jsonb(OLD)-ARRAY['accepted_at','exchange_rate_id','rate_source']) THEN
   RAISE EXCEPTION 'charging quotation snapshots are immutable';
 END IF;
 IF NEW.original_currency <> NEW.wallet_currency AND NOT EXISTS (
   SELECT 1 FROM exchange_rates r WHERE r.id=NEW.exchange_rate_id
    AND r.source_currency=NEW.original_currency AND r.target_currency=NEW.wallet_currency
    AND r.rate=NEW.exchange_rate AND r.source=NEW.rate_source
 ) THEN RAISE EXCEPTION 'accepted exchange rate must match snapshot'; END IF;
 RETURN NEW;
END $$;

-- Only rename the exact seed-owned records still carrying the old seed name.
-- Operator edits and all identifiers, associations and financial records survive.
UPDATE stations s SET name=n.new_name
FROM (VALUES
('stn_demo_george_town','cpo_voltgo','ChargeNet Demo - George Town','VoltGo George Town'),
('stn_demo_gurney','cpo_chargesync','ChargeNet Demo - Gurney Bay','ChargeSync Gurney Bay'),
('stn_demo_tanjung_tokong','cpo_ecowatt','ChargeNet Demo - Tanjung Tokong','EcoWatt Tanjung Tokong'),
('stn_demo_air_itam','cpo_chargesync','ChargeNet Demo - Air Itam','ChargeSync Air Itam'),
('stn_demo_bayan_lepas','cpo_voltgo','ChargeNet Demo - Bayan Lepas / Queensbay area','VoltGo Bayan Lepas / Queensbay area'),
('stn_demo_batu_ferringhi','cpo_ecowatt','ChargeNet Demo - Batu Ferringhi','EcoWatt Batu Ferringhi'),
('stn_demo_butterworth','cpo_voltgo','ChargeNet Demo - Butterworth','VoltGo Butterworth'),
('stn_demo_seberang_jaya','cpo_chargesync','ChargeNet Demo - Seberang Jaya','ChargeSync Seberang Jaya'),
('stn_demo_bukit_mertajam','cpo_ecowatt','ChargeNet Demo - Bukit Mertajam','EcoWatt Bukit Mertajam'),
('stn_demo_juru','cpo_chargesync','ChargeNet Demo - Juru / Auto-City area','ChargeSync Juru / Auto-City area'),
('stn_demo_batu_kawan','cpo_voltgo','ChargeNet Demo - Batu Kawan','VoltGo Batu Kawan'),
('stn_demo_marina_bay','cpo_chargesync','ChargeNet Demo - Marina Bay','ChargeSync Marina Bay'),
('stn_demo_jurong_east','cpo_voltgo','ChargeNet Demo - Jurong East','VoltGo Jurong East'),
('stn_demo_tampines','cpo_ecowatt','ChargeNet Demo - Tampines','EcoWatt Tampines'),
('stn_demo_woodlands','cpo_voltgo','ChargeNet Demo - Woodlands','VoltGo Woodlands'),
('stn_demo_orchard','cpo_ecowatt','ChargeNet Demo - Orchard Road','EcoWatt Orchard Road'),
('stn_demo_hat_yai','cpo_voltgo','ChargeNet Demo - Hat Yai','VoltGo Hat Yai'),
('stn_demo_songkhla','cpo_ecowatt','ChargeNet Demo - Songkhla','EcoWatt Songkhla'),
('stn_demo_phuket','cpo_chargesync','ChargeNet Demo - Phuket Town','ChargeSync Phuket Town'),
('stn_demo_bangkok','cpo_chargesync','ChargeNet Demo - Bangkok','ChargeSync Bangkok'),
('stn_demo_hat_yai_east','cpo_ecowatt','ChargeNet Demo - Hat Yai East','EcoWatt Hat Yai East'),
('stn_demo_songkhla_south','cpo_chargesync','ChargeNet Demo - Songkhla South','ChargeSync Songkhla South'),
('stn_demo_marina_south','cpo_voltgo','ChargeNet Demo - Marina South','VoltGo Marina South'),
('stn_demo_bangkok_west','cpo_voltgo','ChargeNet Demo - Bangkok West','VoltGo Bangkok West')
) AS n(id,cpo_id,old_name,new_name)
WHERE s.id=n.id AND s.cpo_id=n.cpo_id AND s.name=n.old_name;
