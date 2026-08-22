ALTER TABLE exchange_rates ADD CONSTRAINT exchange_rates_finite
 CHECK(rate NOT IN ('NaN'::numeric,'Infinity'::numeric,'-Infinity'::numeric));
CREATE UNIQUE INDEX stations_id_cpo ON stations(id,cpo_id);
CREATE UNIQUE INDEX connectors_id_station ON connectors(id,station_id);
ALTER TABLE charging_sessions ADD CONSTRAINT sessions_station_cpo
 FOREIGN KEY(station_id,cpo_id) REFERENCES stations(id,cpo_id) NOT VALID;
ALTER TABLE charging_sessions ADD CONSTRAINT sessions_connector_station
 FOREIGN KEY(connector_id,station_id) REFERENCES connectors(id,station_id) NOT VALID;
ALTER TABLE cpo_partners ADD CONSTRAINT cpo_supported_country
 CHECK(country_code IN ('MY','SG','TH')) NOT VALID;
-- Legacy invalid relationships are left intact for historical review, while
-- every new or modified relationship is checked. Do not delete history to
-- make an old inconsistency disappear.
