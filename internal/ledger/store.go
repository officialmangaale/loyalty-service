package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

var effectKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Store struct {
	db    *sql.DB
	now   func() time.Time
	newID func() uuid.UUID
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now, newID: uuid.New}
}

func (s *Store) RecordPendingEarn(ctx context.Context, command PendingEarnCommand) (Result, error) {
	if err := validatePendingEarn(command); err != nil {
		return Result{}, err
	}
	metadata := normalizeMetadata(command.Metadata)
	businessKey := fmt.Sprintf("order:%s:earn:pending:%s", command.OrderID, command.EffectKey)
	entry := Entry{
		LedgerID:       s.newID(),
		PlatformUserID: command.PlatformUserID,
		Type:           EntryOrderPendingEarn,
		PendingDelta:   command.Points,
		BusinessKey:    businessKey,
		IdempotencyKey: command.IdempotencyKey,
		ReferenceType:  "ORDER",
		ReferenceID:    command.OrderID,
		OrderID:        stringPointer(command.OrderID),
		RestaurantID:   command.RestaurantID,
		CampaignID:     command.CampaignID,
		FundingSource:  command.FundingSource,
		PolicyVersion:  command.PolicyVersion,
		ExpiresAt:      command.ExpiresAt,
		OccurredAt:     command.OccurredAt,
		ActorType:      ActorSystem,
		Metadata:       metadata,
	}

	var result Result
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		account, err := s.ensureAndLockAccount(ctx, tx, command.PlatformUserID)
		if err != nil {
			return err
		}
		entry.AccountID = account.AccountID

		replayed, existing, err := s.insertEntry(ctx, tx, entry)
		if err != nil {
			return err
		}
		if replayed {
			result = Result{Account: account, Entry: existing, Replayed: true}
			return nil
		}

		account, err = s.updateProjection(ctx, tx, account.AccountID, 0, command.Points, 0, 0, 0, false)
		if err != nil {
			return err
		}
		if err := s.recordEffects(ctx, tx, account, entry); err != nil {
			return err
		}
		result = Result{Account: account, Entry: entry}
		return nil
	})
	return result, err
}

func (s *Store) MakePendingAvailable(ctx context.Context, command PendingTransitionCommand) (Result, error) {
	if err := validatePendingTransition(command); err != nil {
		return Result{}, err
	}
	return s.transitionPending(ctx, command, true)
}

func (s *Store) CancelPending(ctx context.Context, command PendingTransitionCommand) (Result, error) {
	if err := validatePendingTransition(command); err != nil {
		return Result{}, err
	}
	return s.transitionPending(ctx, command, false)
}

func (s *Store) transitionPending(ctx context.Context, command PendingTransitionCommand, makeAvailable bool) (Result, error) {
	metadata := normalizeMetadata(command.Metadata)
	var result Result
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		pending, err := s.findEntryByBusinessKey(ctx, tx, command.PendingBusinessKey)
		if err != nil {
			return err
		}
		if pending.Type != EntryOrderPendingEarn || pending.PendingDelta <= 0 {
			return fmt.Errorf("%w: source is not pending earn", ErrInvalidTransition)
		}
		account, err := s.lockAccount(ctx, tx, pending.AccountID)
		if err != nil {
			return err
		}

		entryType := EntryOrderPendingCancel
		businessSuffix := ":cancelled"
		availableDelta := int64(0)
		lifetimeDelta := int64(0)
		if makeAvailable {
			entryType = EntryOrderAvailable
			businessSuffix = ":available"
			availableDelta = pending.PendingDelta
			lifetimeDelta = pending.PendingDelta
		}
		entry := Entry{
			LedgerID:       s.newID(),
			AccountID:      pending.AccountID,
			PlatformUserID: pending.PlatformUserID,
			Type:           entryType,
			AvailableDelta: availableDelta,
			PendingDelta:   -pending.PendingDelta,
			BusinessKey:    command.PendingBusinessKey + businessSuffix,
			IdempotencyKey: command.IdempotencyKey,
			SourceLedgerID: &pending.LedgerID,
			ReferenceType:  pending.ReferenceType,
			ReferenceID:    pending.ReferenceID,
			OrderID:        pending.OrderID,
			RestaurantID:   pending.RestaurantID,
			CampaignID:     pending.CampaignID,
			FundingSource:  pending.FundingSource,
			PolicyVersion:  pending.PolicyVersion,
			ExpiresAt:      pending.ExpiresAt,
			OccurredAt:     command.OccurredAt,
			ActorType:      normalizedActor(command.ActorType),
			ActorID:        command.ActorID,
			Metadata:       metadata,
		}

		replayed, existing, err := s.insertEntry(ctx, tx, entry)
		if err != nil {
			return err
		}
		if replayed {
			result = Result{Account: account, Entry: existing, Replayed: true}
			return nil
		}

		account, err = s.updateProjection(ctx, tx, account.AccountID, availableDelta, -pending.PendingDelta, 0, 0, lifetimeDelta, false)
		if err != nil {
			return fmt.Errorf("%w: pending earn was already transitioned", ErrInvalidTransition)
		}
		if makeAvailable {
			if err := s.createPointLot(ctx, tx, account.AccountID, entry); err != nil {
				return err
			}
		}
		if err := s.recordEffects(ctx, tx, account, entry); err != nil {
			return err
		}
		result = Result{Account: account, Entry: entry}
		return nil
	})
	return result, err
}

func (s *Store) ReverseAvailableGrant(ctx context.Context, command ReversalCommand) (Result, error) {
	if err := validateReversal(command); err != nil {
		return Result{}, err
	}
	metadata := normalizeMetadata(command.Metadata)
	var result Result
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		grant, err := s.findEntryByBusinessKey(ctx, tx, command.GrantBusinessKey)
		if err != nil {
			return err
		}
		if grant.AvailableDelta <= 0 || (grant.Type != EntryOrderAvailable && grant.Type != EntryExpiration) {
			return fmt.Errorf("%w: source is not an available grant", ErrInvalidTransition)
		}
		account, err := s.lockAccount(ctx, tx, grant.AccountID)
		if err != nil {
			return err
		}
		businessKey := fmt.Sprintf("refund:%s:source:%s", command.RefundReference, grant.LedgerID)
		if existing, found, err := s.findOptionalEntryByBusinessKey(ctx, tx, businessKey); err != nil {
			return err
		} else if found {
			reversedPoints := -existing.AvailableDelta + existing.DebtDelta
			if existing.Type != EntryRefundReversal || existing.AccountID != account.AccountID ||
				existing.IdempotencyKey != command.IdempotencyKey || reversedPoints != command.Points ||
				existing.SourceLedgerID == nil || *existing.SourceLedgerID != grant.LedgerID {
				return ErrIdempotencyConflict
			}
			result = Result{Account: account, Entry: existing, Replayed: true}
			return nil
		}

		var alreadyReversed int64
		if err := tx.QueryRowContext(ctx, `
            SELECT COALESCE(SUM((-available_delta) + debt_delta), 0)
            FROM loyalty.ledger
            WHERE source_ledger_id = $1 AND entry_type = 'REFUND_REVERSAL'`, grant.LedgerID).Scan(&alreadyReversed); err != nil {
			return fmt.Errorf("sum prior reversals: %w", err)
		}
		if alreadyReversed+command.Points > grant.AvailableDelta {
			return fmt.Errorf("%w: reversal exceeds original grant", ErrInvalidTransition)
		}

		lotID, remaining, err := s.lockGrantLot(ctx, tx, grant.LedgerID)
		if err != nil {
			return err
		}
		availableReversal := min(command.Points, remaining)
		debt := command.Points - availableReversal
		entry := Entry{
			LedgerID:       s.newID(),
			AccountID:      grant.AccountID,
			PlatformUserID: grant.PlatformUserID,
			Type:           EntryRefundReversal,
			AvailableDelta: -availableReversal,
			DebtDelta:      debt,
			BusinessKey:    businessKey,
			IdempotencyKey: command.IdempotencyKey,
			SourceLedgerID: &grant.LedgerID,
			ReferenceType:  "REFUND",
			ReferenceID:    command.RefundReference,
			OrderID:        grant.OrderID,
			RestaurantID:   grant.RestaurantID,
			CampaignID:     grant.CampaignID,
			FundingSource:  grant.FundingSource,
			PolicyVersion:  command.PolicyVersion,
			OccurredAt:     command.OccurredAt,
			ActorType:      normalizedActor(command.ActorType),
			ActorID:        command.ActorID,
			Metadata:       metadata,
		}

		replayed, existing, err := s.insertEntry(ctx, tx, entry)
		if err != nil {
			return err
		}
		if replayed {
			result = Result{Account: account, Entry: existing, Replayed: true}
			return nil
		}

		if _, err := tx.ExecContext(ctx, `
            UPDATE loyalty.point_lots
            SET remaining_points = remaining_points - $2,
                status = CASE WHEN remaining_points - $2 = 0 AND reserved_points = 0 THEN 'REVERSED' ELSE status END,
                version = version + 1,
                updated_at = $3
            WHERE lot_id = $1`, lotID, availableReversal, s.now().UTC()); err != nil {
			return fmt.Errorf("apply reversal to point lot: %w", err)
		}
		account, err = s.updateProjection(ctx, tx, account.AccountID, -availableReversal, 0, 0, debt, 0, debt > 0)
		if err != nil {
			return err
		}
		if err := s.recordEffects(ctx, tx, account, entry); err != nil {
			return err
		}
		result = Result{Account: account, Entry: entry}
		return nil
	})
	return result, err
}

func (s *Store) ExpireLot(ctx context.Context, lotID uuid.UUID, idempotencyKey string, occurredAt time.Time) (Result, error) {
	if lotID == uuid.Nil || strings.TrimSpace(idempotencyKey) == "" || occurredAt.IsZero() {
		return Result{}, ErrInvalidCommand
	}
	var result Result
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var accountID uuid.UUID
		if err := tx.QueryRowContext(ctx, `SELECT account_id FROM loyalty.point_lots WHERE lot_id = $1`, lotID).Scan(&accountID); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("find expiry lot: %w", err)
		}
		account, err := s.lockAccount(ctx, tx, accountID)
		if err != nil {
			return err
		}
		businessKey := fmt.Sprintf("lot:%s:expiration", lotID)
		if existing, found, err := s.findOptionalEntryByBusinessKey(ctx, tx, businessKey); err != nil {
			return err
		} else if found {
			if existing.Type != EntryExpiration || existing.AccountID != account.AccountID ||
				existing.IdempotencyKey != idempotencyKey || existing.ReferenceID != lotID.String() {
				return ErrIdempotencyConflict
			}
			result = Result{Account: account, Entry: existing, Replayed: true}
			return nil
		}

		var grantLedgerID uuid.UUID
		var points int64
		var expiresAt sql.NullTime
		var status string
		if err := tx.QueryRowContext(ctx, `
            SELECT grant_ledger_id, remaining_points, expires_at, status
            FROM loyalty.point_lots WHERE lot_id = $1 FOR UPDATE`, lotID).Scan(&grantLedgerID, &points, &expiresAt, &status); err != nil {
			return fmt.Errorf("lock expiry lot: %w", err)
		}
		if status != "ACTIVE" || points == 0 {
			return fmt.Errorf("%w: point lot is not active", ErrInvalidTransition)
		}
		if !expiresAt.Valid || expiresAt.Time.After(occurredAt) {
			return fmt.Errorf("%w: point lot has not expired", ErrInvalidTransition)
		}
		grant, err := s.findEntryByID(ctx, tx, grantLedgerID)
		if err != nil {
			return err
		}
		entry := Entry{
			LedgerID:       s.newID(),
			AccountID:      account.AccountID,
			PlatformUserID: account.PlatformUserID,
			Type:           EntryExpiration,
			AvailableDelta: -points,
			BusinessKey:    businessKey,
			IdempotencyKey: idempotencyKey,
			SourceLedgerID: &grantLedgerID,
			ReferenceType:  "POINT_LOT",
			ReferenceID:    lotID.String(),
			OrderID:        grant.OrderID,
			RestaurantID:   grant.RestaurantID,
			CampaignID:     grant.CampaignID,
			FundingSource:  grant.FundingSource,
			PolicyVersion:  grant.PolicyVersion,
			OccurredAt:     occurredAt,
			ActorType:      ActorSystem,
			Metadata:       json.RawMessage(`{"reason":"scheduled_expiration"}`),
		}
		replayed, existing, err := s.insertEntry(ctx, tx, entry)
		if err != nil {
			return err
		}
		if replayed {
			result = Result{Account: account, Entry: existing, Replayed: true}
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
            UPDATE loyalty.point_lots
            SET remaining_points = 0, status = 'EXPIRED', version = version + 1, updated_at = $2
            WHERE lot_id = $1`, lotID, s.now().UTC()); err != nil {
			return fmt.Errorf("expire point lot: %w", err)
		}
		account, err = s.updateProjection(ctx, tx, account.AccountID, -points, 0, 0, 0, 0, false)
		if err != nil {
			return err
		}
		if err := s.recordEffects(ctx, tx, account, entry); err != nil {
			return err
		}
		result = Result{Account: account, Entry: entry}
		return nil
	})
	return result, err
}

func (s *Store) withSerializableTx(ctx context.Context, operation func(*sql.Tx) error) error {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return fmt.Errorf("begin loyalty transaction: %w", err)
		}
		err = operation(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err == nil {
			return nil
		}
		if !isRetryableTransactionError(err) || attempt == maxAttempts {
			return err
		}
		_ = tx.Rollback()
		delay := time.Duration(attempt*10) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return ErrInvariantViolation
}

func isRetryableTransactionError(err error) bool {
	var pqError *pq.Error
	if !errors.As(err, &pqError) {
		return false
	}
	return pqError.Code == "40001" || pqError.Code == "40P01"
}

func (s *Store) ensureAndLockAccount(ctx context.Context, tx *sql.Tx, platformUserID uuid.UUID) (Account, error) {
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO loyalty.accounts (account_id, platform_user_id)
        VALUES ($1, $2)
        ON CONFLICT (platform_user_id) DO NOTHING`, s.newID(), platformUserID); err != nil {
		return Account{}, fmt.Errorf("ensure loyalty account: %w", err)
	}
	var accountID uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT account_id FROM loyalty.accounts WHERE platform_user_id = $1`, platformUserID).Scan(&accountID); err != nil {
		return Account{}, fmt.Errorf("find loyalty account: %w", err)
	}
	return s.lockAccount(ctx, tx, accountID)
}

func (s *Store) lockAccount(ctx context.Context, tx *sql.Tx, accountID uuid.UUID) (Account, error) {
	row := tx.QueryRowContext(ctx, `
        SELECT account_id, platform_user_id, status, timezone,
               available_points, pending_points, reserved_points, debt_points,
               lifetime_earned_points, version, created_at, updated_at
        FROM loyalty.accounts
        WHERE account_id = $1
        FOR UPDATE`, accountID)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("lock loyalty account: %w", err)
	}
	return account, nil
}

func (s *Store) updateProjection(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, available, pending, reserved, debt, lifetime int64, freeze bool) (Account, error) {
	row := tx.QueryRowContext(ctx, `
        UPDATE loyalty.accounts
        SET available_points = available_points + $2,
            pending_points = pending_points + $3,
            reserved_points = reserved_points + $4,
            debt_points = debt_points + $5,
            lifetime_earned_points = lifetime_earned_points + $6,
            status = CASE WHEN $7 THEN 'DEBT_REVIEW' ELSE status END,
            version = version + 1,
            updated_at = $8
        WHERE account_id = $1
          AND available_points + $2 >= 0
          AND pending_points + $3 >= 0
          AND reserved_points + $4 >= 0
          AND debt_points + $5 >= 0
          AND lifetime_earned_points + $6 >= 0
        RETURNING account_id, platform_user_id, status, timezone,
                  available_points, pending_points, reserved_points, debt_points,
                  lifetime_earned_points, version, created_at, updated_at`,
		accountID, available, pending, reserved, debt, lifetime, freeze, s.now().UTC())
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrInvariantViolation
	}
	if err != nil {
		return Account{}, fmt.Errorf("update loyalty projection: %w", err)
	}
	return account, nil
}

func scanAccount(scanner interface{ Scan(...any) error }) (Account, error) {
	var account Account
	err := scanner.Scan(
		&account.AccountID,
		&account.PlatformUserID,
		&account.Status,
		&account.Timezone,
		&account.AvailablePoints,
		&account.PendingPoints,
		&account.ReservedPoints,
		&account.DebtPoints,
		&account.LifetimeEarnedPoints,
		&account.Version,
		&account.CreatedAt,
		&account.UpdatedAt,
	)
	return account, err
}

func (s *Store) insertEntry(ctx context.Context, tx *sql.Tx, entry Entry) (bool, Entry, error) {
	result, err := tx.ExecContext(ctx, `
        INSERT INTO loyalty.ledger (
            ledger_id, account_id, platform_user_id, entry_type,
            available_delta, pending_delta, reserved_delta, debt_delta,
            business_key, idempotency_key, source_ledger_id,
            reference_type, reference_id, order_id, restaurant_id,
            campaign_id, reward_id, funding_source, policy_version,
            monetary_value_minor, currency, expires_at, occurred_at,
            actor_type, actor_id, metadata
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
            $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26
        ) ON CONFLICT (business_key) DO NOTHING`,
		entry.LedgerID, entry.AccountID, entry.PlatformUserID, entry.Type,
		entry.AvailableDelta, entry.PendingDelta, entry.ReservedDelta, entry.DebtDelta,
		entry.BusinessKey, entry.IdempotencyKey, nullableUUID(entry.SourceLedgerID),
		entry.ReferenceType, entry.ReferenceID, nullableString(entry.OrderID), nullableInt64(entry.RestaurantID),
		nullableUUID(entry.CampaignID), nullableUUID(entry.RewardID), entry.FundingSource, entry.PolicyVersion,
		nullableInt64(entry.MonetaryValueMinor), nullableString(entry.Currency), nullableTime(entry.ExpiresAt), entry.OccurredAt,
		entry.ActorType, nullableString(entry.ActorID), []byte(normalizeMetadata(entry.Metadata)),
	)
	if err != nil {
		var pqError *pq.Error
		if errors.As(err, &pqError) && pqError.Code == "23505" {
			return false, Entry{}, ErrIdempotencyConflict
		}
		return false, Entry{}, fmt.Errorf("insert loyalty ledger entry: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, Entry{}, fmt.Errorf("read ledger insert result: %w", err)
	}
	if rows == 1 {
		return false, Entry{}, nil
	}

	existing, err := s.findEntryByBusinessKey(ctx, tx, entry.BusinessKey)
	if err != nil {
		return false, Entry{}, err
	}
	if !sameEffect(existing, entry) {
		return false, Entry{}, ErrIdempotencyConflict
	}
	return true, existing, nil
}

func (s *Store) findEntryByBusinessKey(ctx context.Context, tx *sql.Tx, businessKey string) (Entry, error) {
	return scanEntry(tx.QueryRowContext(ctx, entrySelect+` WHERE business_key = $1`, businessKey))
}

func (s *Store) findOptionalEntryByBusinessKey(ctx context.Context, tx *sql.Tx, businessKey string) (Entry, bool, error) {
	entry, err := scanEntry(tx.QueryRowContext(ctx, entrySelect+` WHERE business_key = $1`, businessKey))
	if errors.Is(err, ErrNotFound) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return entry, true, nil
}

func (s *Store) findEntryByID(ctx context.Context, tx *sql.Tx, ledgerID uuid.UUID) (Entry, error) {
	return scanEntry(tx.QueryRowContext(ctx, entrySelect+` WHERE ledger_id = $1`, ledgerID))
}

const entrySelect = `
SELECT ledger_id, account_id, platform_user_id, entry_type,
       available_delta, pending_delta, reserved_delta, debt_delta,
       business_key, idempotency_key, source_ledger_id,
       reference_type, reference_id, order_id, restaurant_id,
       campaign_id, reward_id, funding_source, policy_version,
       monetary_value_minor, currency, expires_at, occurred_at,
       actor_type, actor_id, metadata, created_at
FROM loyalty.ledger`

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var entry Entry
	var sourceID, campaignID, rewardID uuid.NullUUID
	var orderID, currency, actorID sql.NullString
	var restaurantID, monetaryValue sql.NullInt64
	var expiresAt sql.NullTime
	var metadata []byte
	err := row.Scan(
		&entry.LedgerID, &entry.AccountID, &entry.PlatformUserID, &entry.Type,
		&entry.AvailableDelta, &entry.PendingDelta, &entry.ReservedDelta, &entry.DebtDelta,
		&entry.BusinessKey, &entry.IdempotencyKey, &sourceID,
		&entry.ReferenceType, &entry.ReferenceID, &orderID, &restaurantID,
		&campaignID, &rewardID, &entry.FundingSource, &entry.PolicyVersion,
		&monetaryValue, &currency, &expiresAt, &entry.OccurredAt,
		&entry.ActorType, &actorID, &metadata, &entry.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("scan loyalty ledger entry: %w", err)
	}
	entry.SourceLedgerID = uuidPointer(sourceID)
	entry.OrderID = stringPointerFromNull(orderID)
	entry.RestaurantID = int64Pointer(restaurantID)
	entry.CampaignID = uuidPointer(campaignID)
	entry.RewardID = uuidPointer(rewardID)
	entry.MonetaryValueMinor = int64Pointer(monetaryValue)
	entry.Currency = stringPointerFromNull(currency)
	entry.ExpiresAt = timePointer(expiresAt)
	entry.ActorID = stringPointerFromNull(actorID)
	entry.Metadata = json.RawMessage(metadata)
	return entry, nil
}

func (s *Store) createPointLot(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, grant Entry) error {
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO loyalty.point_lots (
            lot_id, account_id, grant_ledger_id, original_points,
            remaining_points, expires_at
        ) VALUES ($1, $2, $3, $4, $4, $5)`,
		s.newID(), accountID, grant.LedgerID, grant.AvailableDelta, nullableTime(grant.ExpiresAt)); err != nil {
		return fmt.Errorf("create point lot: %w", err)
	}
	return nil
}

func (s *Store) lockGrantLot(ctx context.Context, tx *sql.Tx, grantLedgerID uuid.UUID) (uuid.UUID, int64, error) {
	var lotID uuid.UUID
	var remaining int64
	err := tx.QueryRowContext(ctx, `
        SELECT lot_id, remaining_points
        FROM loyalty.point_lots
        WHERE grant_ledger_id = $1
        FOR UPDATE`, grantLedgerID).Scan(&lotID, &remaining)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, 0, fmt.Errorf("%w: point lot is missing", ErrInvariantViolation)
	}
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("lock grant point lot: %w", err)
	}
	return lotID, remaining, nil
}

func (s *Store) recordEffects(ctx context.Context, tx *sql.Tx, account Account, entry Entry) error {
	auditMetadata, err := json.Marshal(map[string]any{
		"ledger_id":  entry.LedgerID,
		"entry_type": entry.Type,
	})
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO loyalty.audit_events (
            audit_id, event_type, actor_type, actor_id, subject_type,
            subject_id, external_reference, correlation_id, metadata, occurred_at
        ) VALUES ($1, 'LEDGER_ENTRY_APPENDED', $2, $3, 'LOYALTY_ACCOUNT', $4, $5, $6, $7, $8)`,
		s.newID(), entry.ActorType, nullableString(entry.ActorID), account.AccountID,
		entry.ReferenceID, entry.IdempotencyKey, auditMetadata, entry.OccurredAt); err != nil {
		return fmt.Errorf("append loyalty audit event: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"account_id":       account.AccountID,
		"platform_user_id": account.PlatformUserID,
		"ledger_id":        entry.LedgerID,
		"entry_type":       entry.Type,
		"available_points": account.AvailablePoints,
		"pending_points":   account.PendingPoints,
		"reserved_points":  account.ReservedPoints,
		"debt_points":      account.DebtPoints,
		"account_status":   account.Status,
	})
	if err != nil {
		return fmt.Errorf("encode balance event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO loyalty.outbox_events (
            outbox_id, event_type, event_version, aggregate_type,
            aggregate_id, aggregate_sequence, payload, correlation_id, causation_id
        ) VALUES ($1, 'loyalty.balance.changed', 1, 'LOYALTY_ACCOUNT', $2, $3, $4, $5, $6)`,
		s.newID(), account.AccountID.String(), account.Version, payload, entry.IdempotencyKey, entry.LedgerID.String()); err != nil {
		return fmt.Errorf("append loyalty balance outbox event: %w", err)
	}
	return nil
}

func validatePendingEarn(command PendingEarnCommand) error {
	if command.PlatformUserID == uuid.Nil || strings.TrimSpace(command.OrderID) == "" ||
		len(command.OrderID) > 120 ||
		!effectKeyPattern.MatchString(command.EffectKey) || command.Points <= 0 ||
		strings.TrimSpace(command.PolicyVersion) == "" || len(command.PolicyVersion) > 100 ||
		strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 200 ||
		command.OccurredAt.IsZero() || !validFunding(command.FundingSource) || !validMetadata(command.Metadata) {
		return ErrInvalidCommand
	}
	return nil
}

func validatePendingTransition(command PendingTransitionCommand) error {
	if strings.TrimSpace(command.PendingBusinessKey) == "" || strings.TrimSpace(command.IdempotencyKey) == "" ||
		len(command.PendingBusinessKey) > 180 || len(command.IdempotencyKey) > 200 ||
		command.OccurredAt.IsZero() || !validActor(command.ActorType) || !validMetadata(command.Metadata) {
		return ErrInvalidCommand
	}
	return nil
}

func validateReversal(command ReversalCommand) error {
	if strings.TrimSpace(command.GrantBusinessKey) == "" || strings.TrimSpace(command.RefundReference) == "" ||
		len(command.GrantBusinessKey) > 200 || len(command.RefundReference) > 120 ||
		command.Points <= 0 || strings.TrimSpace(command.IdempotencyKey) == "" ||
		len(command.IdempotencyKey) > 200 || strings.TrimSpace(command.PolicyVersion) == "" ||
		len(command.PolicyVersion) > 100 || command.OccurredAt.IsZero() ||
		!validActor(command.ActorType) || !validMetadata(command.Metadata) {
		return ErrInvalidCommand
	}
	return nil
}

func validFunding(source FundingSource) bool {
	return source == FundingMangaale || source == FundingRestaurant || source == FundingShared || source == FundingNone
}

func validActor(actor ActorType) bool {
	return actor == "" || actor == ActorSystem || actor == ActorAdmin || actor == ActorReconciler
}

func validMetadata(metadata json.RawMessage) bool {
	return len(metadata) == 0 || json.Valid(metadata)
}

func normalizeMetadata(metadata json.RawMessage) json.RawMessage {
	if len(metadata) == 0 {
		return json.RawMessage(`{}`)
	}
	return metadata
}

func normalizedActor(actor ActorType) ActorType {
	if actor == "" {
		return ActorSystem
	}
	return actor
}

func sameEffect(left, right Entry) bool {
	return left.AccountID == right.AccountID &&
		left.Type == right.Type &&
		left.AvailableDelta == right.AvailableDelta &&
		left.PendingDelta == right.PendingDelta &&
		left.ReservedDelta == right.ReservedDelta &&
		left.DebtDelta == right.DebtDelta &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.ReferenceType == right.ReferenceType &&
		left.ReferenceID == right.ReferenceID &&
		left.PolicyVersion == right.PolicyVersion
}

func nullableUUID(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func uuidPointer(value uuid.NullUUID) *uuid.UUID {
	if !value.Valid {
		return nil
	}
	copy := value.UUID
	return &copy
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}

func stringPointerFromNull(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func int64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	copy := value.Int64
	return &copy
}

func timePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	copy := value.Time
	return &copy
}
