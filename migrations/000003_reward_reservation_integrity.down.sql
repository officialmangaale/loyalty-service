DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM loyalty.reservations WHERE reward_id IS NOT NULL) THEN
        RAISE EXCEPTION 'refusing reward reservation rollback while catalog reservations exist';
    END IF;
END
$$;

DROP INDEX IF EXISTS loyalty.reward_redemptions_limit_idx;
DROP INDEX IF EXISTS loyalty.reservations_restaurant_active_idx;
ALTER TABLE loyalty.reservations DROP CONSTRAINT IF EXISTS reservations_reward_restaurant_required_ck;
ALTER TABLE loyalty.reservations DROP CONSTRAINT IF EXISTS reservations_reward_fk;
ALTER TABLE loyalty.reservations DROP COLUMN IF EXISTS restaurant_id;
