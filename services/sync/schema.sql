CREATE TABLE IF NOT EXISTS focusgate_accounts (
    account text PRIMARY KEY,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    retained_from bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS focusgate_entities (
    account text NOT NULL REFERENCES focusgate_accounts(account) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('feed', 'item')),
    id text NOT NULL,
    revision bigint NOT NULL,
    data jsonb NOT NULL,
    published bigint,
    PRIMARY KEY (account, kind, id),
    UNIQUE (account, revision)
);
CREATE INDEX IF NOT EXISTS focusgate_item_retention ON focusgate_entities(account, published DESC, id) WHERE kind = 'item';
CREATE TABLE IF NOT EXISTS focusgate_operations (
    account text NOT NULL REFERENCES focusgate_accounts(account) ON DELETE CASCADE,
    id text NOT NULL,
    fingerprint text NOT NULL,
    ack jsonb NOT NULL,
    received timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account, id)
);
CREATE INDEX IF NOT EXISTS focusgate_operation_retention ON focusgate_operations(account, received);
