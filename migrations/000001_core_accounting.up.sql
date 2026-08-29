CREATE SCHEMA IF NOT EXISTS loyalty;

CREATE TABLE loyalty.accounts (
    account_id uuid PRIMARY KEY,
    platform_user_id uuid NOT NULL UNIQUE,
    status text NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'FROZEN', 'DEBT_REVIEW', 'CLOSED')),
    timezone text NOT NULL DEFAULT 'Asia/Kolkata',
    available_points bigint NOT NULL DEFAULT 0 CHECK (available_points >= 0),
    pending_points bigint NOT NULL DEFAULT 0 CHECK (pending_points >= 0),
    reserved_points bigint NOT NULL DEFAULT 0 CHECK (reserved_points >= 0),
    debt_points bigint NOT NULL DEFAULT 0 CHECK (debt_points >= 0),
    lifetime_earned_points bigint NOT NULL DEFAULT 0
        CHECK (lifetime_earned_points >= 0),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

COMMENT ON TABLE loyalty.accounts IS
    'Rebuildable balance projection and account state. loyalty.ledger is authoritative.';

CREATE TABLE loyalty.ledger (
    ledger_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    platform_user_id uuid NOT NULL,
    entry_type text NOT NULL CHECK (entry_type IN (
        'ORDER_PENDING_EARN',
        'ORDER_EARN_AVAILABLE',
        'ORDER_PENDING_CANCEL',
        'QR_BONUS',
        'FIRST_ORDER_BONUS',
        'REFERRAL_BONUS',
        'MISSION_REWARD',
        'BIRTHDAY_BONUS',
        'TIER_BONUS',
        'REDEMPTION_RESERVE',
        'REDEMPTION_RELEASE',
        'REDEMPTION_CAPTURE',
        'REFUND_REVERSAL',
        'EXPIRATION',
        'ADMIN_ADJUSTMENT',
        'PROMOTION_REVERSAL'
    )),
    available_delta bigint NOT NULL DEFAULT 0,
    pending_delta bigint NOT NULL DEFAULT 0,
    reserved_delta bigint NOT NULL DEFAULT 0,
    debt_delta bigint NOT NULL DEFAULT 0,
    business_key text NOT NULL UNIQUE CHECK (length(business_key) BETWEEN 1 AND 200),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    source_ledger_id uuid REFERENCES loyalty.ledger(ledger_id),
    reference_type text NOT NULL CHECK (length(reference_type) BETWEEN 1 AND 80),
    reference_id text NOT NULL CHECK (length(reference_id) BETWEEN 1 AND 200),
    order_id text,
    restaurant_id bigint,
    campaign_id uuid,
    reward_id uuid,
    funding_source text NOT NULL DEFAULT 'MANGAALE'
        CHECK (funding_source IN ('MANGAALE', 'RESTAURANT', 'SHARED', 'NONE')),
    policy_version text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 100),
    monetary_value_minor bigint CHECK (monetary_value_minor IS NULL OR monetary_value_minor >= 0),
    currency char(3),
    expires_at timestamptz,
    occurred_at timestamptz NOT NULL,
    actor_type text NOT NULL CHECK (actor_type IN ('SYSTEM', 'CUSTOMER', 'ADMIN', 'RESTAURANT', 'RECONCILER')),
    actor_id text,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (
        available_delta <> 0 OR pending_delta <> 0 OR
        reserved_delta <> 0 OR debt_delta <> 0
    ),
    CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
    UNIQUE (account_id, idempotency_key)
);

CREATE INDEX ledger_account_created_idx
    ON loyalty.ledger (account_id, created_at DESC, ledger_id DESC);
CREATE INDEX ledger_order_idx
    ON loyalty.ledger (order_id) WHERE order_id IS NOT NULL;
CREATE INDEX ledger_expiry_idx
    ON loyalty.ledger (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX ledger_campaign_idx
    ON loyalty.ledger (campaign_id) WHERE campaign_id IS NOT NULL;
CREATE INDEX ledger_source_idx
    ON loyalty.ledger (source_ledger_id) WHERE source_ledger_id IS NOT NULL;

CREATE TABLE loyalty.point_lots (
    lot_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    grant_ledger_id uuid NOT NULL UNIQUE REFERENCES loyalty.ledger(ledger_id),
    original_points bigint NOT NULL CHECK (original_points > 0),
    remaining_points bigint NOT NULL CHECK (remaining_points >= 0),
    reserved_points bigint NOT NULL DEFAULT 0 CHECK (reserved_points >= 0),
    expires_at timestamptz,
    status text NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'DEPLETED', 'EXPIRED', 'REVERSED')),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (remaining_points + reserved_points <= original_points)
);

CREATE INDEX point_lots_fifo_idx
    ON loyalty.point_lots (account_id, expires_at NULLS LAST, created_at, lot_id)
    WHERE status = 'ACTIVE' AND remaining_points > 0;

CREATE TABLE loyalty.reservations (
    reservation_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    checkout_reference text NOT NULL CHECK (length(checkout_reference) BETWEEN 1 AND 200),
    order_id text,
    status text NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'CAPTURED', 'RELEASED', 'EXPIRED')),
    points bigint NOT NULL CHECK (points > 0),
    discount_minor bigint NOT NULL CHECK (discount_minor >= 0),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    reward_id uuid,
    policy_version text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 100),
    funding_source text NOT NULL
        CHECK (funding_source IN ('MANGAALE', 'RESTAURANT', 'SHARED')),
    expires_at timestamptz NOT NULL,
    captured_at timestamptz,
    released_at timestamptz,
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (account_id, idempotency_key),
    UNIQUE (account_id, checkout_reference),
    CHECK (expires_at > created_at)
);

CREATE INDEX reservations_active_expiry_idx
    ON loyalty.reservations (expires_at, reservation_id)
    WHERE status = 'ACTIVE';

CREATE TABLE loyalty.reservation_lot_allocations (
    reservation_id uuid NOT NULL REFERENCES loyalty.reservations(reservation_id),
    lot_id uuid NOT NULL REFERENCES loyalty.point_lots(lot_id),
    points bigint NOT NULL CHECK (points > 0),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (reservation_id, lot_id)
);

CREATE TABLE loyalty.inbox_events (
    event_id uuid PRIMARY KEY,
    event_type text NOT NULL CHECK (length(event_type) BETWEEN 1 AND 120),
    event_version integer NOT NULL CHECK (event_version > 0),
    aggregate_type text NOT NULL CHECK (length(aggregate_type) BETWEEN 1 AND 80),
    aggregate_id text NOT NULL CHECK (length(aggregate_id) BETWEEN 1 AND 200),
    aggregate_sequence bigint NOT NULL CHECK (aggregate_sequence > 0),
    status text NOT NULL DEFAULT 'RECEIVED'
        CHECK (status IN ('RECEIVED', 'PROCESSING', 'PROCESSED', 'HELD', 'FAILED')),
    payload jsonb NOT NULL,
    correlation_id text,
    causation_id text,
    received_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    processed_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error_code text,
    last_error_at timestamptz,
    UNIQUE (aggregate_type, aggregate_id, aggregate_sequence)
);

CREATE INDEX inbox_events_work_idx
    ON loyalty.inbox_events (status, received_at, event_id)
    WHERE status IN ('RECEIVED', 'HELD', 'FAILED');

CREATE TABLE loyalty.outbox_events (
    outbox_id uuid PRIMARY KEY,
    event_type text NOT NULL CHECK (length(event_type) BETWEEN 1 AND 120),
    event_version integer NOT NULL CHECK (event_version > 0),
    aggregate_type text NOT NULL CHECK (length(aggregate_type) BETWEEN 1 AND 80),
    aggregate_id text NOT NULL CHECK (length(aggregate_id) BETWEEN 1 AND 200),
    aggregate_sequence bigint NOT NULL CHECK (aggregate_sequence > 0),
    payload jsonb NOT NULL,
    correlation_id text,
    causation_id text,
    status text NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'PUBLISHING', 'PUBLISHED', 'FAILED')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    published_at timestamptz,
    last_error_code text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (aggregate_type, aggregate_id, aggregate_sequence)
);

CREATE INDEX outbox_events_publish_idx
    ON loyalty.outbox_events (available_at, outbox_id)
    WHERE status IN ('PENDING', 'FAILED');

CREATE TABLE loyalty.policies (
    policy_id uuid PRIMARY KEY,
    policy_type text NOT NULL CHECK (length(policy_type) BETWEEN 1 AND 80),
    version text NOT NULL CHECK (length(version) BETWEEN 1 AND 100),
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    configuration jsonb NOT NULL,
    effective_from timestamptz,
    effective_until timestamptz,
    created_by text NOT NULL,
    approved_by text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    activated_at timestamptz,
    UNIQUE (policy_type, version),
    CHECK (effective_until IS NULL OR effective_from IS NULL OR effective_until > effective_from),
    CHECK (status <> 'ACTIVE' OR approved_by IS NOT NULL)
);

CREATE UNIQUE INDEX policies_one_active_type_idx
    ON loyalty.policies (policy_type) WHERE status = 'ACTIVE';

CREATE TABLE loyalty.entitlements (
    entitlement_id uuid PRIMARY KEY,
    feature_key text NOT NULL CHECK (length(feature_key) BETWEEN 1 AND 100),
    restaurant_id bigint,
    cohort_key text NOT NULL DEFAULT 'ALL',
    enabled boolean NOT NULL DEFAULT false,
    configuration jsonb NOT NULL DEFAULT '{}'::jsonb,
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    changed_by text NOT NULL,
    change_reason text NOT NULL CHECK (length(change_reason) BETWEEN 1 AND 500),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE NULLS NOT DISTINCT (feature_key, restaurant_id, cohort_key)
);

CREATE TABLE loyalty.audit_events (
    audit_id uuid PRIMARY KEY,
    event_type text NOT NULL CHECK (length(event_type) BETWEEN 1 AND 120),
    actor_type text NOT NULL CHECK (actor_type IN ('SYSTEM', 'CUSTOMER', 'ADMIN', 'RESTAURANT', 'RECONCILER')),
    actor_id text,
    subject_type text NOT NULL CHECK (length(subject_type) BETWEEN 1 AND 80),
    subject_id text NOT NULL CHECK (length(subject_id) BETWEEN 1 AND 200),
    reason text CHECK (reason IS NULL OR length(reason) <= 1000),
    external_reference text,
    correlation_id text,
    before_state jsonb,
    after_state jsonb,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX audit_events_subject_idx
    ON loyalty.audit_events (subject_type, subject_id, created_at DESC);

CREATE TABLE loyalty.reconciliation_findings (
    finding_id uuid PRIMARY KEY,
    finding_type text NOT NULL CHECK (length(finding_type) BETWEEN 1 AND 120),
    severity text NOT NULL CHECK (severity IN ('INFO', 'WARNING', 'CRITICAL')),
    status text NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN', 'ACKNOWLEDGED', 'RESOLVED', 'FALSE_POSITIVE')),
    account_id uuid REFERENCES loyalty.accounts(account_id),
    reference_type text,
    reference_id text,
    expected_state jsonb,
    actual_state jsonb,
    detected_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    resolved_at timestamptz,
    resolution text,
    UNIQUE (finding_type, reference_type, reference_id, status)
);

CREATE OR REPLACE FUNCTION loyalty.reject_immutable_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION '% is append-only; use a compensating record', TG_TABLE_NAME
        USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER ledger_reject_update_delete
BEFORE UPDATE OR DELETE ON loyalty.ledger
FOR EACH ROW EXECUTE FUNCTION loyalty.reject_immutable_mutation();

CREATE TRIGGER audit_events_reject_update_delete
BEFORE UPDATE OR DELETE ON loyalty.audit_events
FOR EACH ROW EXECUTE FUNCTION loyalty.reject_immutable_mutation();
