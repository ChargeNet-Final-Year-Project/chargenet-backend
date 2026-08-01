ALTER TABLE stations ADD COLUMN region text NOT NULL DEFAULT '';
CREATE INDEX stations_country_region_idx ON stations(country_code,region) WHERE is_active;

-- Administrative areas for the shipped fictional catalogue. Existing operator
-- sites outside these known IDs retain their own data.
UPDATE stations SET region='Penang' WHERE id IN (
 'stn_demo_george_town','stn_demo_gurney','stn_demo_tanjung_tokong',
 'stn_demo_air_itam','stn_demo_bayan_lepas','stn_demo_batu_ferringhi',
 'stn_demo_butterworth','stn_demo_seberang_jaya','stn_demo_bukit_mertajam',
 'stn_demo_juru','stn_demo_batu_kawan');
UPDATE stations SET region=CASE
 WHEN city IN ('Marina Bay','Marina South','Orchard Road') THEN 'Central'
 WHEN city='Jurong East' THEN 'West'
 WHEN city='Tampines' THEN 'East'
 WHEN city='Woodlands' THEN 'North' ELSE region END
 WHERE id LIKE 'stn_demo_%' AND country_code='SG';
UPDATE stations SET region=CASE
 WHEN city IN ('Hat Yai','Hat Yai East','Songkhla','Songkhla South') THEN 'Songkhla'
 WHEN city='Phuket Town' THEN 'Phuket'
 WHEN city IN ('Bangkok','Bangkok West') THEN 'Bangkok' ELSE region END
 WHERE id LIKE 'stn_demo_%' AND country_code='TH';
UPDATE stations SET region=CASE
 WHEN city='Kuala Lumpur' THEN 'Kuala Lumpur'
 WHEN city='Putrajaya' THEN 'Putrajaya'
 WHEN city='Seremban' THEN 'Negeri Sembilan'
 WHEN city='Genting Sempah' THEN 'Pahang'
 WHEN city IN ('Petaling Jaya','Subang Jaya','Cyberjaya','Sungai Buloh') THEN 'Selangor'
 ELSE region END
 WHERE id IN ('stn_vlt_sgb','stn_vlt_gsr','stn_vlt_srr','stn_vlt_pjs',
 'stn_csy_mvl','stn_csy_pav','stn_csy_1u','stn_csy_swp','stn_csy_ioi',
 'stn_eco_klc','stn_eco_bsg','stn_eco_ttd','stn_eco_cyb','stn_eco_bbg');
