DROP TABLE IF EXISTS session_events, tenant_deks;

-- One data key per tenant per month, wrapped by a key version that lives in
-- the KMS. A row here is safe in a backup: without its key version it is
-- random bytes.
CREATE TABLE tenant_deks (
    tenant_id   text        NOT NULL,
    month       date        NOT NULL,
    kms_version text        NOT NULL,
    wrapped_dek bytea       NOT NULL,
    PRIMARY KEY (tenant_id, month)
);

-- Agent session events. Ids and timestamps stay in clear so we can query and
-- partition; the content is encrypted with the tenant's key for that month.
CREATE TABLE session_events (
    tenant_id  text        NOT NULL,
    session_id text        NOT NULL,
    seq        int         NOT NULL,
    created_at timestamptz NOT NULL,
    content    bytea       NOT NULL,
    PRIMARY KEY (tenant_id, session_id, seq, created_at)
) PARTITION BY RANGE (created_at);
