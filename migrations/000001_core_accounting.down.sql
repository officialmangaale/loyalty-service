DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM loyalty.ledger LIMIT 1) THEN
        RAISE EXCEPTION 'refusing core rollback: loyalty ledger contains accounting history';
    END IF;
    IF EXISTS (SELECT 1 FROM loyalty.audit_events LIMIT 1) THEN
        RAISE EXCEPTION 'refusing core rollback: loyalty audit history exists';
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS audit_events_reject_update_delete ON loyalty.audit_events;
DROP TRIGGER IF EXISTS ledger_reject_update_delete ON loyalty.ledger;
DROP FUNCTION IF EXISTS loyalty.reject_immutable_mutation();
DROP TABLE IF EXISTS loyalty.reconciliation_findings;
DROP TABLE IF EXISTS loyalty.audit_events;
DROP TABLE IF EXISTS loyalty.entitlements;
DROP TABLE IF EXISTS loyalty.policies;
DROP TABLE IF EXISTS loyalty.outbox_events;
DROP TABLE IF EXISTS loyalty.inbox_events;
DROP TABLE IF EXISTS loyalty.reservation_lot_allocations;
DROP TABLE IF EXISTS loyalty.reservations;
DROP TABLE IF EXISTS loyalty.point_lots;
DROP TABLE IF EXISTS loyalty.ledger;
DROP TABLE IF EXISTS loyalty.accounts;
