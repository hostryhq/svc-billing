-- Third migration (forward-only, expand only): go-common sync.Schema additions for personal data with salted commitments and
-- cloud-signed erasure (ADR-0014 A2.1 item 7, O2-8, O2-12). sync_pii keeps the erasable values and their salts and the
-- commitments that survive an erasure; sync_pii_erased records the erased ops so that a late arrival of their data is
-- discarded. billing_app also updates sync_pii (an erasure empties values and salts, it does not delete the row).
ALTER TABLE sync_pii ADD COLUMN IF NOT EXISTS salts   jsonb NOT NULL DEFAULT '{}';
ALTER TABLE sync_pii ADD COLUMN IF NOT EXISTS commits jsonb NOT NULL DEFAULT '{}';
CREATE TABLE IF NOT EXISTS sync_pii_erased (
  tenant_id text   NOT NULL,
  origin    text   NOT NULL,
  stream    text   NOT NULL,
  seq       bigint NOT NULL,
  PRIMARY KEY (tenant_id, origin, stream, seq)
);
ALTER TABLE sync_pii_erased ENABLE ROW LEVEL SECURITY;
ALTER TABLE sync_pii_erased FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON sync_pii_erased;
CREATE POLICY tenant_isolation ON sync_pii_erased TO billing_app
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
GRANT SELECT, INSERT, UPDATE ON sync_pii TO billing_app;
GRANT SELECT, INSERT ON sync_pii_erased TO billing_app;
