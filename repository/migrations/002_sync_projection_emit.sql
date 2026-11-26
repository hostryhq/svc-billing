-- Second migration (forward-only, expand only): the projection engine's tables (go-common sync.Schema), the emit_queue
-- outbox of cloud-origin emissions (go-common emit.QueueSchema) and the op key of a rule-set emission (ADR-0014 A2.1,
-- A2.2). The two schemas are copied from go-common as that package asks; a change there is a new migration here.

CREATE TABLE IF NOT EXISTS sync_candidates (
  tenant_id text   NOT NULL,
  entity    text   NOT NULL,
  row_id    text   NOT NULL,
  origin    text   NOT NULL,
  stream    text   NOT NULL,
  seq       bigint NOT NULL,
  idx       int    NOT NULL,
  hlc       text   NOT NULL,
  kind      text   NOT NULL,
  fields    jsonb  NOT NULL DEFAULT '{}',
  prev      jsonb  NOT NULL DEFAULT '{}',
  PRIMARY KEY (tenant_id, entity, row_id, origin, stream, seq, idx)
);
CREATE INDEX IF NOT EXISTS sync_candidates_op ON sync_candidates (tenant_id, origin, stream, seq);
CREATE TABLE IF NOT EXISTS sync_voids (
  tenant_id text   NOT NULL,
  origin    text   NOT NULL,
  stream    text   NOT NULL,
  seq       bigint NOT NULL,
  PRIMARY KEY (tenant_id, origin, stream, seq)
);
CREATE TABLE IF NOT EXISTS sync_pii (
  tenant_id text   NOT NULL,
  entity    text   NOT NULL,
  row_id    text   NOT NULL,
  origin    text   NOT NULL,
  stream    text   NOT NULL,
  seq       bigint NOT NULL,
  fields    jsonb  NOT NULL,
  PRIMARY KEY (tenant_id, entity, row_id, origin, stream, seq)
);

CREATE TABLE IF NOT EXISTS emit_queue (
  id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id       text        NOT NULL,
  key             text        NOT NULL,
  op              jsonb       NOT NULL,
  state           text        NOT NULL DEFAULT 'pending',
  attempts        int         NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  last_error      text        NOT NULL DEFAULT '',
  origin          text,
  stream          text,
  seq             bigint,
  replayed        boolean,
  created_at      timestamptz NOT NULL,
  sent_at         timestamptz,
  UNIQUE (tenant_id, key)
);
CREATE INDEX IF NOT EXISTS emit_queue_pending ON emit_queue (tenant_id, id) WHERE state = 'pending';

-- A rule-set emission names its queue key; acked_at stays (unused from here) until a later contract release.
ALTER TABLE rule_set_emissions ADD COLUMN IF NOT EXISTS op_key text NOT NULL DEFAULT '';

-- Row-level security. The projection tables are tenant-owned (billing_app, the tenant of the transaction). emit_queue is
-- written by the tenant transaction for its own tenant only (insert, and the key columns of its own rows) and drained by the global role, which sends
-- every tenant's rows in order and reads the sent state back for the rule-set counts.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['sync_candidates', 'sync_voids', 'sync_pii']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I TO billing_app
            USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t);
    END LOOP;
END $$;

ALTER TABLE emit_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE emit_queue FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_insert ON emit_queue FOR INSERT TO billing_app
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
-- ON CONFLICT (tenant_id, key) DO NOTHING (go-common emit.Enqueue) needs to read the arbiter columns: the tenant role reads
-- those two columns of its own rows and nothing else (never the op body).
CREATE POLICY tenant_keys ON emit_queue FOR SELECT TO billing_app
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY global_all ON emit_queue TO billing_global USING (true) WITH CHECK (true);

GRANT SELECT, INSERT ON sync_candidates, sync_voids TO billing_app;
GRANT SELECT, INSERT, DELETE ON sync_pii TO billing_app;
GRANT INSERT ON emit_queue TO billing_app;
GRANT SELECT (tenant_id, key) ON emit_queue TO billing_app;
GRANT SELECT, INSERT, UPDATE ON emit_queue TO billing_global;
