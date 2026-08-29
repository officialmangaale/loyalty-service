ALTER TABLE loyalty.reservations
    ADD COLUMN restaurant_id bigint;

ALTER TABLE loyalty.reservations
    ADD CONSTRAINT reservations_reward_fk
    FOREIGN KEY (reward_id) REFERENCES loyalty.reward_catalog(reward_id);

ALTER TABLE loyalty.reservations
    ADD CONSTRAINT reservations_reward_restaurant_required_ck
    CHECK (reward_id IS NULL OR restaurant_id IS NOT NULL);

CREATE INDEX reservations_restaurant_active_idx
    ON loyalty.reservations (restaurant_id, expires_at, reservation_id)
    WHERE status = 'ACTIVE';

CREATE INDEX reward_redemptions_limit_idx
    ON loyalty.reward_redemptions (reward_id, status, account_id, created_at);
