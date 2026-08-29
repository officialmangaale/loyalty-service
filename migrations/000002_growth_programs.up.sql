CREATE TABLE loyalty.reward_catalog (
    reward_id uuid PRIMARY KEY,
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z0-9][A-Z0-9_-]{1,63}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    description text NOT NULL DEFAULT '',
    reward_type text NOT NULL CHECK (reward_type IN (
        'FIXED_DISCOUNT', 'PERCENTAGE_DISCOUNT', 'FREE_ITEM',
        'FREE_DELIVERY', 'BONUS_POINTS', 'PLATFORM_COUPON'
    )),
    scope text NOT NULL CHECK (scope IN ('GLOBAL_MANGAALE', 'RESTAURANT')),
    restaurant_id bigint,
    points_cost bigint NOT NULL CHECK (points_cost > 0),
    effect jsonb NOT NULL,
    funding_source text NOT NULL
        CHECK (funding_source IN ('MANGAALE', 'RESTAURANT', 'SHARED')),
    policy_version text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 100),
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'RETIRED')),
    starts_at timestamptz,
    ends_at timestamptz,
    per_user_limit integer CHECK (per_user_limit IS NULL OR per_user_limit > 0),
    total_limit bigint CHECK (total_limit IS NULL OR total_limit > 0),
    created_by text NOT NULL,
    approved_by text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((scope = 'RESTAURANT') = (restaurant_id IS NOT NULL)),
    CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at > starts_at),
    CHECK (status <> 'ACTIVE' OR approved_by IS NOT NULL)
);

CREATE INDEX reward_catalog_active_idx
    ON loyalty.reward_catalog (scope, restaurant_id, starts_at, ends_at)
    WHERE status = 'ACTIVE';

CREATE TABLE loyalty.campaigns (
    campaign_id uuid PRIMARY KEY,
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z0-9][A-Z0-9_-]{1,63}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    campaign_type text NOT NULL CHECK (campaign_type IN (
        'EARN_MULTIPLIER', 'FIRST_ORDER', 'FIRST_QR', 'MISSION',
        'REFERRAL', 'BIRTHDAY', 'WIN_BACK', 'PLATFORM_COUPON',
        'FREE_ITEM', 'CASHBACK'
    )),
    scope text NOT NULL CHECK (scope IN ('GLOBAL_MANGAALE', 'RESTAURANT')),
    restaurant_id bigint,
    funding_source text NOT NULL
        CHECK (funding_source IN ('MANGAALE', 'RESTAURANT', 'SHARED')),
    mangaale_funding_bps integer NOT NULL DEFAULT 10000
        CHECK (mangaale_funding_bps BETWEEN 0 AND 10000),
    restaurant_funding_bps integer NOT NULL DEFAULT 0
        CHECK (restaurant_funding_bps BETWEEN 0 AND 10000),
    eligibility_rule jsonb NOT NULL DEFAULT '{}'::jsonb,
    reward_rule jsonb NOT NULL DEFAULT '{}'::jsonb,
    stacking_rule jsonb NOT NULL DEFAULT '{}'::jsonb,
    policy_version text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 100),
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'SCHEDULED', 'ACTIVE', 'PAUSED', 'COMPLETED', 'CANCELLED')),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    timezone text NOT NULL DEFAULT 'Asia/Kolkata',
    per_user_limit integer CHECK (per_user_limit IS NULL OR per_user_limit > 0),
    total_limit bigint CHECK (total_limit IS NULL OR total_limit > 0),
    created_by text NOT NULL,
    approved_by text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((scope = 'RESTAURANT') = (restaurant_id IS NOT NULL)),
    CHECK (mangaale_funding_bps + restaurant_funding_bps = 10000),
    CHECK (ends_at > starts_at),
    CHECK (status NOT IN ('SCHEDULED', 'ACTIVE') OR approved_by IS NOT NULL)
);

CREATE INDEX campaigns_active_idx
    ON loyalty.campaigns (campaign_type, starts_at, ends_at)
    WHERE status IN ('SCHEDULED', 'ACTIVE');

CREATE TABLE loyalty.campaign_budgets (
    campaign_id uuid NOT NULL REFERENCES loyalty.campaigns(campaign_id),
    period_start date NOT NULL,
    period_end date NOT NULL,
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    limit_minor bigint NOT NULL CHECK (limit_minor >= 0),
    reserved_minor bigint NOT NULL DEFAULT 0 CHECK (reserved_minor >= 0),
    consumed_minor bigint NOT NULL DEFAULT 0 CHECK (consumed_minor >= 0),
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (campaign_id, period_start),
    CHECK (period_end >= period_start),
    CHECK (reserved_minor + consumed_minor <= limit_minor)
);

CREATE TABLE loyalty.reward_redemptions (
    redemption_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    reward_id uuid NOT NULL REFERENCES loyalty.reward_catalog(reward_id),
    reservation_id uuid NOT NULL UNIQUE REFERENCES loyalty.reservations(reservation_id),
    order_id text,
    status text NOT NULL DEFAULT 'RESERVED'
        CHECK (status IN ('RESERVED', 'CAPTURED', 'RELEASED', 'REVERSED')),
    points bigint NOT NULL CHECK (points > 0),
    benefit_snapshot jsonb NOT NULL,
    policy_version text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX reward_redemptions_account_idx
    ON loyalty.reward_redemptions (account_id, created_at DESC);

CREATE TABLE loyalty.platform_coupons (
    coupon_id uuid PRIMARY KEY,
    campaign_id uuid REFERENCES loyalty.campaigns(campaign_id),
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z0-9][A-Z0-9_-]{2,39}$'),
    discount_rule jsonb NOT NULL,
    eligibility_rule jsonb NOT NULL DEFAULT '{}'::jsonb,
    stacking_rule jsonb NOT NULL DEFAULT '{}'::jsonb,
    funding_source text NOT NULL
        CHECK (funding_source IN ('MANGAALE', 'SHARED')),
    policy_version text NOT NULL,
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'RETIRED')),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    total_limit bigint CHECK (total_limit IS NULL OR total_limit > 0),
    per_user_limit integer CHECK (per_user_limit IS NULL OR per_user_limit > 0),
    created_by text NOT NULL,
    approved_by text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (ends_at > starts_at),
    CHECK (status <> 'ACTIVE' OR approved_by IS NOT NULL)
);

COMMENT ON TABLE loyalty.platform_coupons IS
    'Platform/global coupons only. Restaurant coupons remain owned by restaurant-service.';

CREATE TABLE loyalty.platform_coupon_redemptions (
    coupon_redemption_id uuid PRIMARY KEY,
    coupon_id uuid NOT NULL REFERENCES loyalty.platform_coupons(coupon_id),
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    order_id text NOT NULL,
    status text NOT NULL DEFAULT 'RESERVED'
        CHECK (status IN ('RESERVED', 'CAPTURED', 'RELEASED', 'REVERSED')),
    discount_minor bigint NOT NULL CHECK (discount_minor >= 0),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    funding_snapshot jsonb NOT NULL,
    idempotency_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (coupon_id, account_id, order_id),
    UNIQUE (account_id, idempotency_key)
);

CREATE TABLE loyalty.tiers (
    tier_id uuid PRIMARY KEY,
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z][A-Z0-9_]{1,31}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    rank integer NOT NULL UNIQUE CHECK (rank >= 0),
    qualification_rule jsonb NOT NULL,
    benefits jsonb NOT NULL DEFAULT '{}'::jsonb,
    policy_version text NOT NULL,
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE loyalty.account_tiers (
    account_tier_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    tier_id uuid NOT NULL REFERENCES loyalty.tiers(tier_id),
    status text NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'EXPIRED', 'SUPERSEDED')),
    qualified_from timestamptz NOT NULL,
    qualified_until timestamptz,
    qualification_snapshot jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (qualified_until IS NULL OR qualified_until > qualified_from)
);

CREATE UNIQUE INDEX account_tiers_one_active_idx
    ON loyalty.account_tiers (account_id) WHERE status = 'ACTIVE';

CREATE TABLE loyalty.missions (
    mission_id uuid PRIMARY KEY,
    campaign_id uuid REFERENCES loyalty.campaigns(campaign_id),
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z0-9][A-Z0-9_-]{1,63}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    target_rule jsonb NOT NULL,
    eligibility_rule jsonb NOT NULL DEFAULT '{}'::jsonb,
    reward_rule jsonb NOT NULL,
    max_completions integer NOT NULL DEFAULT 1 CHECK (max_completions > 0),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    timezone text NOT NULL DEFAULT 'Asia/Kolkata',
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'COMPLETED')),
    policy_version text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (ends_at > starts_at)
);

CREATE TABLE loyalty.mission_progress (
    mission_id uuid NOT NULL REFERENCES loyalty.missions(mission_id),
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    progress jsonb NOT NULL DEFAULT '{}'::jsonb,
    completion_count integer NOT NULL DEFAULT 0 CHECK (completion_count >= 0),
    status text NOT NULL DEFAULT 'IN_PROGRESS'
        CHECK (status IN ('IN_PROGRESS', 'COMPLETED', 'EXPIRED')),
    last_event_id uuid,
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    started_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (mission_id, account_id)
);

CREATE TABLE loyalty.streaks (
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    streak_key text NOT NULL CHECK (length(streak_key) BETWEEN 1 AND 100),
    current_count integer NOT NULL DEFAULT 0 CHECK (current_count >= 0),
    best_count integer NOT NULL DEFAULT 0 CHECK (best_count >= 0),
    last_qualified_local_date date,
    timezone text NOT NULL DEFAULT 'Asia/Kolkata',
    version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, streak_key),
    CHECK (best_count >= current_count)
);

CREATE TABLE loyalty.referrals (
    referral_id uuid PRIMARY KEY,
    campaign_id uuid REFERENCES loyalty.campaigns(campaign_id),
    referrer_account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    referee_platform_user_id uuid NOT NULL,
    referee_account_id uuid REFERENCES loyalty.accounts(account_id),
    referral_code text NOT NULL,
    status text NOT NULL DEFAULT 'REGISTERED'
        CHECK (status IN ('REGISTERED', 'QUALIFIED', 'REWARDED', 'REVERSED', 'REJECTED')),
    qualifying_order_id text,
    fraud_decision_id uuid,
    qualified_at timestamptz,
    rewarded_at timestamptz,
    reversed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE NULLS NOT DISTINCT (campaign_id, referee_platform_user_id),
    CHECK (referrer_account_id IS DISTINCT FROM referee_account_id)
);

CREATE INDEX referrals_referrer_idx
    ON loyalty.referrals (referrer_account_id, created_at DESC);

CREATE TABLE loyalty.birthday_issuances (
    birthday_issuance_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    campaign_id uuid NOT NULL REFERENCES loyalty.campaigns(campaign_id),
    birthday_year integer NOT NULL CHECK (birthday_year BETWEEN 1900 AND 9999),
    consent_version text NOT NULL,
    source_reference text NOT NULL,
    status text NOT NULL DEFAULT 'ISSUED'
        CHECK (status IN ('ISSUED', 'REDEEMED', 'EXPIRED', 'REVERSED')),
    issued_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (account_id, campaign_id, birthday_year)
);

CREATE TABLE loyalty.segments (
    segment_id uuid PRIMARY KEY,
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    deterministic_rule jsonb NOT NULL,
    policy_version text NOT NULL,
    status text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE loyalty.account_segments (
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    segment_id uuid NOT NULL REFERENCES loyalty.segments(segment_id),
    status text NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE', 'EXPIRED')),
    explanation jsonb NOT NULL,
    qualified_at timestamptz NOT NULL,
    expires_at timestamptz,
    PRIMARY KEY (account_id, segment_id),
    CHECK (expires_at IS NULL OR expires_at > qualified_at)
);

CREATE TABLE loyalty.fraud_signals (
    fraud_signal_id uuid PRIMARY KEY,
    account_id uuid REFERENCES loyalty.accounts(account_id),
    signal_type text NOT NULL CHECK (length(signal_type) BETWEEN 1 AND 100),
    token_key_version text NOT NULL,
    tokenized_value text NOT NULL,
    risk_weight integer NOT NULL CHECK (risk_weight BETWEEN -1000 AND 1000),
    source_reference text,
    retention_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (signal_type, token_key_version, tokenized_value, source_reference)
);

CREATE INDEX fraud_signals_account_idx
    ON loyalty.fraud_signals (account_id, created_at DESC)
    WHERE account_id IS NOT NULL;

CREATE TABLE loyalty.fraud_decisions (
    fraud_decision_id uuid PRIMARY KEY,
    account_id uuid REFERENCES loyalty.accounts(account_id),
    reference_type text NOT NULL,
    reference_id text NOT NULL,
    risk_score integer NOT NULL,
    decision text NOT NULL CHECK (decision IN ('ALLOW', 'REVIEW', 'DENY')),
    reasons jsonb NOT NULL,
    policy_version text NOT NULL,
    decided_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (reference_type, reference_id, policy_version)
);

ALTER TABLE loyalty.referrals
    ADD CONSTRAINT referrals_fraud_decision_fk
    FOREIGN KEY (fraud_decision_id) REFERENCES loyalty.fraud_decisions(fraud_decision_id);

CREATE TABLE loyalty.admin_adjustment_requests (
    adjustment_request_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES loyalty.accounts(account_id),
    points_delta bigint NOT NULL CHECK (points_delta <> 0),
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    external_reference text NOT NULL,
    requested_by text NOT NULL,
    status text NOT NULL DEFAULT 'DELAYED'
        CHECK (status IN ('DELAYED', 'EFFECTIVE', 'CANCELLED', 'REFUSED')),
    effective_at timestamptz NOT NULL,
    cancelled_at timestamptz,
    refusal_reason text,
    ledger_id uuid REFERENCES loyalty.ledger(ledger_id),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (requested_by, external_reference)
);

CREATE TABLE loyalty.promotion_decisions (
    promotion_decision_id uuid PRIMARY KEY,
    account_id uuid REFERENCES loyalty.accounts(account_id),
    restaurant_id bigint NOT NULL,
    checkout_reference text NOT NULL,
    order_id text,
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    subtotal_minor bigint NOT NULL CHECK (subtotal_minor >= 0),
    total_discount_minor bigint NOT NULL CHECK (total_discount_minor >= 0),
    mangaale_funded_minor bigint NOT NULL CHECK (mangaale_funded_minor >= 0),
    restaurant_funded_minor bigint NOT NULL CHECK (restaurant_funded_minor >= 0),
    customer_payable_minor bigint NOT NULL CHECK (customer_payable_minor >= 0),
    adjustments jsonb NOT NULL,
    calculation_base jsonb NOT NULL,
    stacking_policy_version text NOT NULL,
    idempotency_key text NOT NULL,
    decided_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (restaurant_id, checkout_reference),
    UNIQUE (account_id, idempotency_key),
    CHECK (mangaale_funded_minor + restaurant_funded_minor = total_discount_minor),
    CHECK (total_discount_minor <= subtotal_minor)
);

CREATE TRIGGER promotion_decisions_reject_update_delete
BEFORE UPDATE OR DELETE ON loyalty.promotion_decisions
FOR EACH ROW EXECUTE FUNCTION loyalty.reject_immutable_mutation();

CREATE TABLE loyalty.campaign_metrics_daily (
    campaign_id uuid NOT NULL REFERENCES loyalty.campaigns(campaign_id),
    metric_date date NOT NULL,
    impressions bigint NOT NULL DEFAULT 0 CHECK (impressions >= 0),
    qualified bigint NOT NULL DEFAULT 0 CHECK (qualified >= 0),
    redemptions bigint NOT NULL DEFAULT 0 CHECK (redemptions >= 0),
    reversals bigint NOT NULL DEFAULT 0 CHECK (reversals >= 0),
    issued_points bigint NOT NULL DEFAULT 0 CHECK (issued_points >= 0),
    redeemed_points bigint NOT NULL DEFAULT 0 CHECK (redeemed_points >= 0),
    cost_minor bigint NOT NULL DEFAULT 0 CHECK (cost_minor >= 0),
    attributed_revenue_minor bigint NOT NULL DEFAULT 0 CHECK (attributed_revenue_minor >= 0),
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (campaign_id, metric_date)
);
