-- Billing service schema, first migration (forward-only). Runs as the migrator role, which owns every table; the runtime
-- roles must already exist (scripts/provision-roles.sql):
--   billing_app     DML on the tenant-owned tables, NOSUPERUSER NOBYPASSRLS, not an owner (the runtime role)
--   billing_global  DML on the service-global tables only (rule master, platform vendor log, grants, emissions)
--   billing_relay   the outbox table only (relay to the bus)
--
-- Table classes (ADR-0003 section 2, ADR-0014 A1.4 item 2): tenant-owned (standard policy), service-internal
-- (outbox, inbox, idempotency keys), service-global (no tenant). Projected read models are documents (docs): one row
-- per (tenant, kind, id) holding the API representation; the facts behind them are immutable ops.

CREATE TABLE docs (
    tenant_id uuid  NOT NULL,
    kind      text  NOT NULL,
    id        text  NOT NULL,
    outlet    text  NOT NULL DEFAULT '',
    date      text  NOT NULL DEFAULT '',
    sort      text  NOT NULL DEFAULT '',
    k1        text  NOT NULL DEFAULT '',
    k2        text  NOT NULL DEFAULT '',
    stamp     text  NOT NULL DEFAULT '',
    body      jsonb NOT NULL,
    PRIMARY KEY (tenant_id, kind, id)
);
CREATE INDEX docs_list ON docs (tenant_id, kind, outlet, sort DESC, id DESC);
CREATE INDEX docs_date ON docs (tenant_id, kind, date);
CREATE INDEX docs_k1 ON docs (tenant_id, kind, k1);

-- An op is applied once: identity (origin, stream, seq) (ADR-0011 E3, ADR-0002 S5).
CREATE TABLE applied_ops (
    tenant_id uuid   NOT NULL,
    origin    text   NOT NULL,
    stream    text   NOT NULL,
    seq       bigint NOT NULL,
    hash      text   NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, origin, stream, seq)
);

CREATE TABLE idempotency_keys (
    tenant_id   uuid        NOT NULL,
    op          text        NOT NULL,
    key         text        NOT NULL,
    fingerprint text        NOT NULL,
    response    bytea,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, op, key)
);

CREATE TABLE event_keys (
    tenant_id uuid   NOT NULL,
    key       text   NOT NULL,
    last_seq  bigint NOT NULL,
    PRIMARY KEY (tenant_id, key)
);

-- Tenant-scoped vendor access log (support sessions reading billing data, ADR-0013 V5).
CREATE TABLE vendor_access_log (
    tenant_id          uuid        NOT NULL,
    id                 uuid        NOT NULL,
    occurred_at        timestamptz NOT NULL,
    vendor_user        text        NOT NULL,
    token_type         text        NOT NULL,
    support_session_id text,
    action_key         text        NOT NULL,
    target_id          text,
    ticket_reference   text,
    outcome            text        NOT NULL CHECK (outcome IN ('ok', 'refused')),
    result_count       int,
    request_id         text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);

-- go-common outbox and inbox schemas (ADR-0011 B4, B5); tenant_id is text in the outbox.
CREATE TABLE outbox (
    id           uuid        PRIMARY KEY,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    type         text        NOT NULL,
    subject      text        NOT NULL,
    tenant_id    text        NOT NULL,
    key          text        NOT NULL DEFAULT '',
    key_seq      bigint      NOT NULL DEFAULT 0,
    payload      bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished ON outbox (seq) WHERE published_at IS NULL;
CREATE INDEX outbox_published_at ON outbox (published_at) WHERE published_at IS NOT NULL;
CREATE TABLE outbox_relay_state (
    stream     text        PRIMARY KEY,
    generation text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE inbox (
    consumer     text        NOT NULL,
    event_id     uuid        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
CREATE TABLE event_parked (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    consumer    text        NOT NULL,
    event_id    text        NOT NULL,
    subject     text        NOT NULL,
    payload     bytea       NOT NULL,
    error       text        NOT NULL,
    attempts    bigint      NOT NULL,
    parked_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);

-- ---------------------------------------------------------------- service-global tables (billing_global)
CREATE TABLE tax_rule_masters (
    id         uuid        PRIMARY KEY,
    version    int         NOT NULL UNIQUE,
    revision   int         NOT NULL,
    state      text        NOT NULL CHECK (state IN ('draft', 'signed', 'published')),
    created_at timestamptz NOT NULL,
    body       jsonb       NOT NULL
);
CREATE TABLE used_grants (
    grant_id text        PRIMARY KEY,
    used_at  timestamptz NOT NULL
);
CREATE TABLE known_tenants (
    tenant_id uuid        PRIMARY KEY,
    added_at  timestamptz NOT NULL
);
CREATE TABLE rule_set_emissions (
    rule_set_id uuid        NOT NULL,
    tenant_id   uuid        NOT NULL,
    acked_at    timestamptz,
    PRIMARY KEY (rule_set_id, tenant_id)
);
CREATE TABLE global_idempotency_keys (
    op          text        NOT NULL,
    key         text        NOT NULL,
    fingerprint text        NOT NULL,
    response    bytea,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (op, key)
);
CREATE TABLE global_event_keys (
    key      text   PRIMARY KEY,
    last_seq bigint NOT NULL
);
-- Vendor actions that name no organisation (the rule-set routes): append-only, ids and fixed-pattern values only.
CREATE TABLE platform_vendor_access_log (
    id                 uuid        PRIMARY KEY,
    occurred_at        timestamptz NOT NULL,
    vendor_user        text        NOT NULL,
    token_type         text        NOT NULL,
    support_session_id text,
    action_key         text        NOT NULL,
    target_id          text,
    ticket_reference   text,
    outcome            text        NOT NULL CHECK (outcome IN ('ok', 'refused')),
    result_count       int,
    request_id         text        NOT NULL DEFAULT ''
);
CREATE INDEX platform_vendor_access_log_time ON platform_vendor_access_log (occurred_at);

-- ---------------------------------------------------------------- row-level security
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['docs', 'applied_ops', 'idempotency_keys', 'event_keys', 'vendor_access_log']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- The outbox: tenant rows for the application role, platform rows for the global role, everything for the relay.
ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON outbox TO billing_app
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY platform_rows ON outbox TO billing_global
    USING (tenant_id = 'platform') WITH CHECK (tenant_id = 'platform');
CREATE POLICY relay_all ON outbox TO billing_relay USING (true) WITH CHECK (true);

-- ---------------------------------------------------------------- privileges
GRANT SELECT, INSERT, UPDATE, DELETE ON docs TO billing_app;
GRANT SELECT, INSERT, UPDATE ON idempotency_keys, event_keys TO billing_app;
GRANT SELECT, INSERT ON applied_ops, vendor_access_log, outbox TO billing_app;
GRANT SELECT, INSERT, DELETE ON inbox TO billing_app;
GRANT SELECT, INSERT ON event_parked TO billing_app;
GRANT SELECT, INSERT, UPDATE ON tax_rule_masters, rule_set_emissions, global_idempotency_keys, global_event_keys TO billing_global;
GRANT DELETE ON tax_rule_masters TO billing_global;
GRANT SELECT, INSERT ON used_grants, known_tenants, platform_vendor_access_log, outbox TO billing_global;
GRANT SELECT, UPDATE, DELETE ON outbox TO billing_relay;
GRANT SELECT, INSERT, UPDATE ON outbox_relay_state TO billing_relay;
