package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

type lotAllocation struct {
	lotID     uuid.UUID
	points    int64
	expiresAt *time.Time
}

// FindReservationByIdempotency returns the current reservation state for an
// exact account-scoped retry. It deliberately does not consult the mutable
// reward catalog: once accepted, an idempotent request must remain replayable
// even if the catalog entry is later paused or edited.
func (s *Store) FindReservationByIdempotency(
	ctx context.Context,
	platformUserID uuid.UUID,
	idempotencyKey string,
) (Reservation, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if platformUserID == uuid.Nil || idempotencyKey == "" || len(idempotencyKey) > 200 {
		return Reservation{}, false, ErrInvalidCommand
	}
	reservation, err := scanReservation(s.db.QueryRowContext(ctx, reservationSelect+`
		WHERE account_id = (
			SELECT account_id FROM loyalty.accounts WHERE platform_user_id = $1
		) AND idempotency_key = $2`,
		platformUserID, idempotencyKey))
	if errors.Is(err, ErrNotFound) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	return reservation, true, nil
}

func (s *Store) ReservePoints(ctx context.Context, command ReserveCommand) (ReservationResult, error) {
	command.Currency = strings.ToUpper(strings.TrimSpace(command.Currency))
	if err := validateReserve(command); err != nil {
		return ReservationResult{}, err
	}
	metadata := normalizeMetadata(command.Metadata)
	var result ReservationResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		account, err := s.ensureAndLockAccount(ctx, tx, command.PlatformUserID)
		if err != nil {
			return err
		}
		if account.Status != "ACTIVE" || account.DebtPoints > 0 {
			return ErrAccountRestricted
		}

		if existing, found, err := s.findOptionalReservationByIdempotency(ctx, tx, account.AccountID, command.IdempotencyKey); err != nil {
			return err
		} else if found {
			if !sameReservationRequest(existing, command) {
				return ErrIdempotencyConflict
			}
			entry, err := s.findEntryByBusinessKey(ctx, tx, reservationBusinessKey(existing.ReservationID, "reserve"))
			if err != nil {
				return fmt.Errorf("load reservation ledger replay: %w", err)
			}
			result = ReservationResult{Account: account, Reservation: existing, Entry: entry, Replayed: true}
			return nil
		}
		if account.AvailablePoints < command.Points {
			return ErrInsufficientPoints
		}
		if command.RewardID != nil {
			if err := s.validateCatalogReward(ctx, tx, account.AccountID, command); err != nil {
				return err
			}
		}

		allocations, err := s.allocateLots(ctx, tx, account.AccountID, command.Points, command.OccurredAt)
		if err != nil {
			return err
		}
		reservation := Reservation{
			ReservationID:     s.newID(),
			AccountID:         account.AccountID,
			IdempotencyKey:    command.IdempotencyKey,
			CheckoutReference: command.CheckoutReference,
			OrderID:           command.OrderID,
			RestaurantID:      command.RestaurantID,
			Status:            "ACTIVE",
			Points:            command.Points,
			DiscountMinor:     command.DiscountMinor,
			Currency:          command.Currency,
			RewardID:          command.RewardID,
			PolicyVersion:     command.PolicyVersion,
			FundingSource:     command.FundingSource,
			ExpiresAt:         command.ExpiresAt,
			CreatedAt:         s.now().UTC(),
			UpdatedAt:         s.now().UTC(),
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO loyalty.reservations (
                reservation_id, account_id, idempotency_key, checkout_reference,
				order_id, restaurant_id, points, discount_minor, currency, reward_id,
				policy_version, funding_source, expires_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`,
			reservation.ReservationID, reservation.AccountID, reservation.IdempotencyKey,
			reservation.CheckoutReference, nullableString(reservation.OrderID), nullableInt64(reservation.RestaurantID), reservation.Points,
			reservation.DiscountMinor, reservation.Currency, nullableUUID(reservation.RewardID),
			reservation.PolicyVersion, reservation.FundingSource, reservation.ExpiresAt, reservation.CreatedAt); err != nil {
			return mapReservationWriteError(err)
		}
		if command.RewardID != nil {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO loyalty.reward_redemptions (
					redemption_id, account_id, reward_id, reservation_id, order_id,
					points, benefit_snapshot, policy_version
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				s.newID(), reservation.AccountID, *command.RewardID, reservation.ReservationID,
				nullableString(command.OrderID), command.Points, command.BenefitSnapshot, command.PolicyVersion); err != nil {
				return fmt.Errorf("create reward redemption: %w", err)
			}
		}

		for _, allocation := range allocations {
			updated, err := tx.ExecContext(ctx, `
                UPDATE loyalty.point_lots
                SET remaining_points = remaining_points - $2,
                    reserved_points = reserved_points + $2,
                    version = version + 1,
                    updated_at = $3
                WHERE lot_id = $1 AND remaining_points >= $2`,
				allocation.lotID, allocation.points, s.now().UTC())
			if err != nil {
				return fmt.Errorf("reserve point lot: %w", err)
			}
			rows, err := updated.RowsAffected()
			if err != nil || rows != 1 {
				return ErrInvariantViolation
			}
			if _, err := tx.ExecContext(ctx, `
                INSERT INTO loyalty.reservation_lot_allocations (reservation_id, lot_id, points)
                VALUES ($1, $2, $3)`, reservation.ReservationID, allocation.lotID, allocation.points); err != nil {
				return fmt.Errorf("record reservation lot allocation: %w", err)
			}
		}

		entry := Entry{
			LedgerID:           s.newID(),
			AccountID:          account.AccountID,
			PlatformUserID:     account.PlatformUserID,
			Type:               EntryRedemptionReserve,
			AvailableDelta:     -command.Points,
			ReservedDelta:      command.Points,
			BusinessKey:        reservationBusinessKey(reservation.ReservationID, "reserve"),
			IdempotencyKey:     command.IdempotencyKey,
			ReferenceType:      "REDEMPTION_RESERVATION",
			ReferenceID:        reservation.ReservationID.String(),
			OrderID:            command.OrderID,
			RestaurantID:       command.RestaurantID,
			RewardID:           command.RewardID,
			FundingSource:      command.FundingSource,
			PolicyVersion:      command.PolicyVersion,
			MonetaryValueMinor: &command.DiscountMinor,
			Currency:           &command.Currency,
			ExpiresAt:          &command.ExpiresAt,
			OccurredAt:         command.OccurredAt,
			ActorType:          ActorSystem,
			Metadata:           metadata,
		}
		if replayed, _, err := s.insertEntry(ctx, tx, entry); err != nil {
			return err
		} else if replayed {
			return ErrInvariantViolation
		}
		account, err = s.updateProjection(ctx, tx, account.AccountID, -command.Points, 0, command.Points, 0, 0, false)
		if err != nil {
			return err
		}
		if err := s.recordEffects(ctx, tx, account, entry); err != nil {
			return err
		}
		result = ReservationResult{Account: account, Reservation: reservation, Entry: entry}
		return nil
	})
	return result, err
}

func (s *Store) ReleaseReservation(ctx context.Context, command ReservationTransitionCommand) (ReservationResult, error) {
	return s.transitionReservation(ctx, command, "RELEASED")
}

func (s *Store) CaptureReservation(ctx context.Context, command ReservationTransitionCommand) (ReservationResult, error) {
	return s.transitionReservation(ctx, command, "CAPTURED")
}

func (s *Store) ExpireReservation(ctx context.Context, command ReservationTransitionCommand) (ReservationResult, error) {
	return s.transitionReservation(ctx, command, "EXPIRED")
}

func (s *Store) transitionReservation(ctx context.Context, command ReservationTransitionCommand, targetStatus string) (ReservationResult, error) {
	if err := validateReservationTransition(command); err != nil {
		return ReservationResult{}, err
	}
	if targetStatus != "RELEASED" && targetStatus != "CAPTURED" && targetStatus != "EXPIRED" {
		return ReservationResult{}, ErrInvalidCommand
	}
	metadata := normalizeMetadata(command.Metadata)
	var result ReservationResult
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var accountID uuid.UUID
		if err := tx.QueryRowContext(ctx, `
            SELECT account_id FROM loyalty.reservations WHERE reservation_id = $1`, command.ReservationID).Scan(&accountID); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("find reservation account: %w", err)
		}
		account, err := s.lockAccount(ctx, tx, accountID)
		if err != nil {
			return err
		}
		reservation, err := s.lockReservation(ctx, tx, command.ReservationID)
		if err != nil {
			return err
		}
		suffix := strings.ToLower(targetStatus)
		businessKey := reservationBusinessKey(reservation.ReservationID, suffix)
		if existing, found, err := s.findOptionalEntryByBusinessKey(ctx, tx, businessKey); err != nil {
			return err
		} else if found {
			if existing.IdempotencyKey != command.IdempotencyKey || existing.AccountID != account.AccountID ||
				(command.OrderID != nil && !sameStringPointer(existing.OrderID, command.OrderID)) {
				return ErrIdempotencyConflict
			}
			result = ReservationResult{Account: account, Reservation: reservation, Entry: existing, Replayed: true}
			return nil
		}
		if reservation.Status != "ACTIVE" {
			return fmt.Errorf("%w: reservation is %s", ErrInvalidTransition, reservation.Status)
		}
		if command.OrderID != nil {
			if reservation.OrderID != nil && *reservation.OrderID != *command.OrderID {
				return ErrIdempotencyConflict
			}
			reservation.OrderID = command.OrderID
		}
		if targetStatus == "EXPIRED" && reservation.ExpiresAt.After(command.OccurredAt) {
			return fmt.Errorf("%w: reservation has not expired", ErrInvalidTransition)
		}

		allocations, err := s.lockReservationAllocations(ctx, tx, reservation.ReservationID)
		if err != nil {
			return err
		}
		var allocatedTotal int64
		for _, allocation := range allocations {
			allocatedTotal += allocation.points
		}
		if allocatedTotal != reservation.Points {
			return fmt.Errorf("%w: reservation allocation total mismatch", ErrInvariantViolation)
		}

		entryType := EntryRedemptionCapture
		availableDelta := int64(0)
		debtDelta := int64(0)
		if targetStatus != "CAPTURED" {
			entryType = EntryRedemptionRelease
			debtRemaining := account.DebtPoints
			for _, allocation := range allocations {
				expired := allocation.expiresAt != nil && !allocation.expiresAt.After(command.OccurredAt)
				restore, debtForLot := releaseAllocationEffect(allocation.points, debtRemaining, expired)
				if err := s.releaseLotAllocation(ctx, tx, allocation, restore, expired); err != nil {
					return err
				}
				debtRemaining -= debtForLot
				debtDelta -= debtForLot
				availableDelta += restore
			}
		} else {
			for _, allocation := range allocations {
				if err := s.captureLotAllocation(ctx, tx, allocation); err != nil {
					return err
				}
			}
		}

		reserveEntry, err := s.findEntryByBusinessKey(ctx, tx, reservationBusinessKey(reservation.ReservationID, "reserve"))
		if err != nil {
			return fmt.Errorf("find reservation source ledger: %w", err)
		}
		entry := Entry{
			LedgerID:           s.newID(),
			AccountID:          account.AccountID,
			PlatformUserID:     account.PlatformUserID,
			Type:               entryType,
			AvailableDelta:     availableDelta,
			ReservedDelta:      -reservation.Points,
			DebtDelta:          debtDelta,
			BusinessKey:        businessKey,
			IdempotencyKey:     command.IdempotencyKey,
			SourceLedgerID:     &reserveEntry.LedgerID,
			ReferenceType:      "REDEMPTION_RESERVATION",
			ReferenceID:        reservation.ReservationID.String(),
			OrderID:            reservation.OrderID,
			RestaurantID:       reservation.RestaurantID,
			RewardID:           reservation.RewardID,
			FundingSource:      reservation.FundingSource,
			PolicyVersion:      reservation.PolicyVersion,
			MonetaryValueMinor: &reservation.DiscountMinor,
			Currency:           &reservation.Currency,
			OccurredAt:         command.OccurredAt,
			ActorType:          normalizedActor(command.ActorType),
			ActorID:            command.ActorID,
			Metadata:           metadata,
		}
		if replayed, _, err := s.insertEntry(ctx, tx, entry); err != nil {
			return err
		} else if replayed {
			return ErrInvariantViolation
		}
		account, err = s.updateProjection(ctx, tx, account.AccountID, availableDelta, 0, -reservation.Points, debtDelta, 0, false)
		if err != nil {
			return err
		}

		transitionedAt := command.OccurredAt
		reservation.Status = targetStatus
		reservation.Version++
		reservation.UpdatedAt = s.now().UTC()
		if targetStatus == "CAPTURED" {
			reservation.CapturedAt = &transitionedAt
		} else {
			reservation.ReleasedAt = &transitionedAt
		}
		updated, err := tx.ExecContext(ctx, `
            UPDATE loyalty.reservations
            SET status = $2,
                captured_at = CASE WHEN $2 = 'CAPTURED' THEN $3 ELSE captured_at END,
                released_at = CASE WHEN $2 IN ('RELEASED', 'EXPIRED') THEN $3 ELSE released_at END,
                version = version + 1,
				order_id = COALESCE(order_id, $5),
				updated_at = $4
            WHERE reservation_id = $1 AND status = 'ACTIVE'`,
			reservation.ReservationID, targetStatus, command.OccurredAt, reservation.UpdatedAt, nullableString(command.OrderID))
		if err != nil {
			return fmt.Errorf("transition reservation: %w", err)
		}
		if reservation.RewardID != nil {
			rewardStatus := targetStatus
			if targetStatus == "EXPIRED" {
				rewardStatus = "RELEASED"
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE loyalty.reward_redemptions
				SET status = $2, order_id = COALESCE(order_id, $3), updated_at = $4
				WHERE reservation_id = $1 AND status = 'RESERVED'`,
				reservation.ReservationID, rewardStatus, nullableString(reservation.OrderID), reservation.UpdatedAt)
			if err != nil {
				return fmt.Errorf("transition reward redemption: %w", err)
			}
			rows, err := result.RowsAffected()
			if err != nil || rows != 1 {
				return ErrInvariantViolation
			}
		}
		rows, err := updated.RowsAffected()
		if err != nil || rows != 1 {
			return ErrInvalidTransition
		}
		if err := s.recordEffects(ctx, tx, account, entry); err != nil {
			return err
		}
		result = ReservationResult{Account: account, Reservation: reservation, Entry: entry}
		return nil
	})
	return result, err
}

func releaseAllocationEffect(points, debtRemaining int64, expired bool) (restore, debtApplied int64) {
	if expired {
		return 0, 0
	}
	debtApplied = min(debtRemaining, points)
	return points - debtApplied, debtApplied
}

func (s *Store) allocateLots(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, points int64, asOf time.Time) ([]lotAllocation, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT lot_id, remaining_points
        FROM loyalty.point_lots
        WHERE account_id = $1
          AND status = 'ACTIVE'
          AND remaining_points > 0
          AND (expires_at IS NULL OR expires_at > $2)
        ORDER BY expires_at NULLS LAST, created_at, lot_id
        FOR UPDATE`, accountID, asOf)
	if err != nil {
		return nil, fmt.Errorf("lock available point lots: %w", err)
	}
	defer rows.Close()

	remaining := points
	var allocations []lotAllocation
	for rows.Next() && remaining > 0 {
		var lotID uuid.UUID
		var lotPoints int64
		if err := rows.Scan(&lotID, &lotPoints); err != nil {
			return nil, fmt.Errorf("scan available point lot: %w", err)
		}
		allocated := min(remaining, lotPoints)
		allocations = append(allocations, lotAllocation{lotID: lotID, points: allocated})
		remaining -= allocated
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate available point lots: %w", err)
	}
	if remaining != 0 {
		return nil, ErrInvariantViolation
	}
	return allocations, nil
}

func (s *Store) lockReservationAllocations(ctx context.Context, tx *sql.Tx, reservationID uuid.UUID) ([]lotAllocation, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT allocation.lot_id, allocation.points, lot.expires_at
        FROM loyalty.reservation_lot_allocations AS allocation
        JOIN loyalty.point_lots AS lot ON lot.lot_id = allocation.lot_id
        WHERE allocation.reservation_id = $1
        ORDER BY lot.expires_at NULLS LAST, lot.created_at, lot.lot_id
        FOR UPDATE OF lot`, reservationID)
	if err != nil {
		return nil, fmt.Errorf("lock reservation allocations: %w", err)
	}
	defer rows.Close()
	var allocations []lotAllocation
	for rows.Next() {
		var allocation lotAllocation
		var expiresAt sql.NullTime
		if err := rows.Scan(&allocation.lotID, &allocation.points, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan reservation allocation: %w", err)
		}
		if expiresAt.Valid {
			value := expiresAt.Time
			allocation.expiresAt = &value
		}
		allocations = append(allocations, allocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reservation allocations: %w", err)
	}
	return allocations, nil
}

func (s *Store) releaseLotAllocation(ctx context.Context, tx *sql.Tx, allocation lotAllocation, restore int64, expired bool) error {
	result, err := tx.ExecContext(ctx, `
        UPDATE loyalty.point_lots
        SET remaining_points = remaining_points + $3,
            reserved_points = reserved_points - $2,
			status = CASE
				WHEN $4 THEN 'EXPIRED'
				WHEN remaining_points + $3 > 0 THEN 'ACTIVE'
				ELSE 'DEPLETED'
			END,
            version = version + 1,
			updated_at = $5
        WHERE lot_id = $1 AND reserved_points >= $2`,
		allocation.lotID, allocation.points, restore, expired, s.now().UTC())
	if err != nil {
		return fmt.Errorf("release point lot allocation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrInvariantViolation
	}
	return nil
}

func (s *Store) captureLotAllocation(ctx context.Context, tx *sql.Tx, allocation lotAllocation) error {
	result, err := tx.ExecContext(ctx, `
        UPDATE loyalty.point_lots
        SET reserved_points = reserved_points - $2,
            status = CASE WHEN remaining_points = 0 AND reserved_points - $2 = 0 THEN 'DEPLETED' ELSE status END,
            version = version + 1,
            updated_at = $3
        WHERE lot_id = $1 AND reserved_points >= $2`,
		allocation.lotID, allocation.points, s.now().UTC())
	if err != nil {
		return fmt.Errorf("capture point lot allocation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrInvariantViolation
	}
	return nil
}

func (s *Store) lockReservation(ctx context.Context, tx *sql.Tx, reservationID uuid.UUID) (Reservation, error) {
	return scanReservation(tx.QueryRowContext(ctx, reservationSelect+` WHERE reservation_id = $1 FOR UPDATE`, reservationID))
}

func (s *Store) findOptionalReservationByIdempotency(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, idempotencyKey string) (Reservation, bool, error) {
	reservation, err := scanReservation(tx.QueryRowContext(ctx, reservationSelect+` WHERE account_id = $1 AND idempotency_key = $2`, accountID, idempotencyKey))
	if errors.Is(err, ErrNotFound) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, err
	}
	return reservation, true, nil
}

const reservationSelect = `
SELECT reservation_id, account_id, idempotency_key, checkout_reference,
       order_id, restaurant_id, status, points, discount_minor, currency, reward_id,
       policy_version, funding_source, expires_at, captured_at, released_at,
       version, created_at, updated_at
FROM loyalty.reservations`

func scanReservation(row interface{ Scan(...any) error }) (Reservation, error) {
	var reservation Reservation
	var orderID sql.NullString
	var restaurantID sql.NullInt64
	var rewardID uuid.NullUUID
	var capturedAt, releasedAt sql.NullTime
	err := row.Scan(
		&reservation.ReservationID, &reservation.AccountID, &reservation.IdempotencyKey,
		&reservation.CheckoutReference, &orderID, &restaurantID, &reservation.Status, &reservation.Points,
		&reservation.DiscountMinor, &reservation.Currency, &rewardID, &reservation.PolicyVersion,
		&reservation.FundingSource, &reservation.ExpiresAt, &capturedAt, &releasedAt,
		&reservation.Version, &reservation.CreatedAt, &reservation.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, ErrNotFound
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("scan loyalty reservation: %w", err)
	}
	reservation.OrderID = stringPointerFromNull(orderID)
	if restaurantID.Valid {
		value := restaurantID.Int64
		reservation.RestaurantID = &value
	}
	reservation.RewardID = uuidPointer(rewardID)
	reservation.CapturedAt = timePointer(capturedAt)
	reservation.ReleasedAt = timePointer(releasedAt)
	return reservation, nil
}

func validateReserve(command ReserveCommand) error {
	if command.PlatformUserID == uuid.Nil || strings.TrimSpace(command.CheckoutReference) == "" ||
		len(command.CheckoutReference) > 200 || command.Points <= 0 || command.DiscountMinor < 0 ||
		!currencyPattern.MatchString(command.Currency) || strings.TrimSpace(command.PolicyVersion) == "" ||
		len(command.PolicyVersion) > 100 || !validFunding(command.FundingSource) || command.FundingSource == FundingNone ||
		strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 200 ||
		command.OccurredAt.IsZero() || !command.ExpiresAt.After(command.OccurredAt) ||
		command.ExpiresAt.Sub(command.OccurredAt) > 15*time.Minute || !validMetadata(command.Metadata) {
		return ErrInvalidCommand
	}
	if command.RewardID != nil && (command.RestaurantID == nil || *command.RestaurantID <= 0 || !validMetadata(command.BenefitSnapshot)) {
		return ErrInvalidCommand
	}
	return nil
}

func validateReservationTransition(command ReservationTransitionCommand) error {
	if command.ReservationID == uuid.Nil || strings.TrimSpace(command.IdempotencyKey) == "" ||
		len(command.IdempotencyKey) > 200 || command.OccurredAt.IsZero() ||
		!validActor(command.ActorType) || !validMetadata(command.Metadata) {
		return ErrInvalidCommand
	}
	if command.OrderID != nil && (strings.TrimSpace(*command.OrderID) == "" || len(*command.OrderID) > 200) {
		return ErrInvalidCommand
	}
	return nil
}

func sameReservationRequest(reservation Reservation, command ReserveCommand) bool {
	return reservation.CheckoutReference == command.CheckoutReference &&
		reservation.Points == command.Points &&
		reservation.DiscountMinor == command.DiscountMinor &&
		reservation.Currency == command.Currency &&
		reservation.PolicyVersion == command.PolicyVersion &&
		reservation.FundingSource == command.FundingSource &&
		sameUUIDPointer(reservation.RewardID, command.RewardID) &&
		sameInt64Pointer(reservation.RestaurantID, command.RestaurantID) &&
		sameStringPointer(reservation.OrderID, command.OrderID)
}

func (s *Store) validateCatalogReward(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, command ReserveCommand) error {
	var pointsCost int64
	var effect []byte
	var fundingSource, policyVersion, scope, status string
	var restaurantID sql.NullInt64
	var startsAt, endsAt sql.NullTime
	var perUserLimit sql.NullInt64
	var totalLimit sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT points_cost, effect, funding_source, policy_version, scope,
		       restaurant_id, status, starts_at, ends_at, per_user_limit, total_limit
		FROM loyalty.reward_catalog
		WHERE reward_id = $1
		FOR UPDATE`, *command.RewardID).Scan(
		&pointsCost, &effect, &fundingSource, &policyVersion, &scope,
		&restaurantID, &status, &startsAt, &endsAt, &perUserLimit, &totalLimit,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRewardUnavailable
	}
	if err != nil {
		return fmt.Errorf("lock reward catalog: %w", err)
	}
	if status != "ACTIVE" || (startsAt.Valid && startsAt.Time.After(command.OccurredAt)) ||
		(endsAt.Valid && !endsAt.Time.After(command.OccurredAt)) || pointsCost != command.Points ||
		fundingSource != string(command.FundingSource) || policyVersion != command.PolicyVersion ||
		!sameJSON(effect, command.BenefitSnapshot) {
		return ErrRewardUnavailable
	}
	if scope == "RESTAURANT" {
		if !restaurantID.Valid || command.RestaurantID == nil || restaurantID.Int64 != *command.RestaurantID {
			return ErrRewardUnavailable
		}
	} else if scope != "GLOBAL_MANGAALE" {
		return ErrRewardUnavailable
	}
	var perUserCount, totalCount int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE account_id = $2), COUNT(*)
		FROM loyalty.reward_redemptions
		WHERE reward_id = $1 AND status IN ('RESERVED', 'CAPTURED')`,
		*command.RewardID, accountID).Scan(&perUserCount, &totalCount); err != nil {
		return fmt.Errorf("read reward usage limits: %w", err)
	}
	if (perUserLimit.Valid && perUserCount >= perUserLimit.Int64) ||
		(totalLimit.Valid && totalCount >= totalLimit.Int64) {
		return ErrUsageLimitReached
	}
	return nil
}

func sameJSON(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func reservationBusinessKey(reservationID uuid.UUID, action string) string {
	return fmt.Sprintf("reservation:%s:%s", reservationID, action)
}

func mapReservationWriteError(err error) error {
	if isUniqueViolation(err) {
		return ErrIdempotencyConflict
	}
	return fmt.Errorf("create loyalty reservation: %w", err)
}

func isUniqueViolation(err error) bool {
	var pqError interface{ SQLState() string }
	return errors.As(err, &pqError) && pqError.SQLState() == "23505"
}

func sameUUIDPointer(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameStringPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
