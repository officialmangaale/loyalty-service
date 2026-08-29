DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM loyalty.promotion_decisions LIMIT 1) THEN
        RAISE EXCEPTION 'refusing growth rollback: promotion decisions exist';
    END IF;
    IF EXISTS (SELECT 1 FROM loyalty.reward_redemptions LIMIT 1) THEN
        RAISE EXCEPTION 'refusing growth rollback: reward redemptions exist';
    END IF;
    IF EXISTS (SELECT 1 FROM loyalty.platform_coupon_redemptions LIMIT 1) THEN
        RAISE EXCEPTION 'refusing growth rollback: coupon redemptions exist';
    END IF;
END;
$$;

DROP TABLE IF EXISTS loyalty.campaign_metrics_daily;
DROP TRIGGER IF EXISTS promotion_decisions_reject_update_delete ON loyalty.promotion_decisions;
DROP TABLE IF EXISTS loyalty.promotion_decisions;
DROP TABLE IF EXISTS loyalty.admin_adjustment_requests;
ALTER TABLE loyalty.referrals DROP CONSTRAINT IF EXISTS referrals_fraud_decision_fk;
DROP TABLE IF EXISTS loyalty.fraud_decisions;
DROP TABLE IF EXISTS loyalty.fraud_signals;
DROP TABLE IF EXISTS loyalty.account_segments;
DROP TABLE IF EXISTS loyalty.segments;
DROP TABLE IF EXISTS loyalty.birthday_issuances;
DROP TABLE IF EXISTS loyalty.referrals;
DROP TABLE IF EXISTS loyalty.streaks;
DROP TABLE IF EXISTS loyalty.mission_progress;
DROP TABLE IF EXISTS loyalty.missions;
DROP TABLE IF EXISTS loyalty.account_tiers;
DROP TABLE IF EXISTS loyalty.tiers;
DROP TABLE IF EXISTS loyalty.platform_coupon_redemptions;
DROP TABLE IF EXISTS loyalty.platform_coupons;
DROP TABLE IF EXISTS loyalty.reward_redemptions;
DROP TABLE IF EXISTS loyalty.campaign_budgets;
DROP TABLE IF EXISTS loyalty.campaigns;
DROP TABLE IF EXISTS loyalty.reward_catalog;
