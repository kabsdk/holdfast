-- No plaintext escrow bundle (decision 047). Each host's password is in escrow/<fqdn>.age in the bucket, encrypted to
-- the recovery key, and the maintenance key may read it back; a rebuilt server brings hosts back from there.
ALTER TABLE host_credentials DROP COLUMN escrow_export_id;
DROP TABLE escrow_exports;
