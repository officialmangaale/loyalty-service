package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *Store) GetAccount(ctx context.Context, platformUserID uuid.UUID) (Account, error) {
	if platformUserID == uuid.Nil {
		return Account{}, ErrInvalidCommand
	}
	row := s.db.QueryRowContext(ctx, `
        SELECT account_id, platform_user_id, status, timezone,
               available_points, pending_points, reserved_points, debt_points,
               lifetime_earned_points, version, created_at, updated_at
        FROM loyalty.accounts
        WHERE platform_user_id = $1`, platformUserID)
	account, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("read loyalty account: %w", err)
	}
	return account, nil
}

func (s *Store) ListEntries(ctx context.Context, platformUserID uuid.UUID, limit int, cursor *EntryCursor) (EntryPage, error) {
	if platformUserID == uuid.Nil {
		return EntryPage{}, ErrInvalidCommand
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	arguments := []any{platformUserID, limit + 1}
	where := "WHERE platform_user_id = $1"
	if cursor != nil {
		if cursor.CreatedAt.IsZero() || cursor.LedgerID == uuid.Nil {
			return EntryPage{}, ErrInvalidCommand
		}
		where += " AND (created_at < $3 OR (created_at = $3 AND ledger_id < $4))"
		arguments = append(arguments, cursor.CreatedAt, cursor.LedgerID)
	}

	rows, err := s.db.QueryContext(ctx, entrySelect+" "+where+" ORDER BY created_at DESC, ledger_id DESC LIMIT $2", arguments...)
	if err != nil {
		return EntryPage{}, fmt.Errorf("list loyalty entries: %w", err)
	}
	defer rows.Close()

	entries := make([]Entry, 0, limit+1)
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return EntryPage{}, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return EntryPage{}, fmt.Errorf("iterate loyalty entries: %w", err)
	}

	page := EntryPage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		last := page.Entries[len(page.Entries)-1]
		page.NextCursor = &EntryCursor{CreatedAt: last.CreatedAt, LedgerID: last.LedgerID}
	}
	return page, nil
}

func (s *Store) GetReservation(ctx context.Context, reservationID uuid.UUID) (Reservation, error) {
	if reservationID == uuid.Nil {
		return Reservation{}, ErrInvalidCommand
	}
	return scanReservation(s.db.QueryRowContext(ctx, reservationSelect+` WHERE reservation_id = $1`, reservationID))
}

func (s *Store) ListExpiredReservationIDs(ctx context.Context, asOf time.Time, limit int) ([]uuid.UUID, error) {
	if asOf.IsZero() {
		return nil, ErrInvalidCommand
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT reservation_id
        FROM loyalty.reservations
        WHERE status = 'ACTIVE' AND expires_at <= $1
        ORDER BY expires_at, reservation_id
        LIMIT $2`, asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired reservations: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired reservation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired reservations: %w", err)
	}
	return ids, nil
}

func (s *Store) ListExpirableLotIDs(ctx context.Context, asOf time.Time, limit int) ([]uuid.UUID, error) {
	if asOf.IsZero() {
		return nil, ErrInvalidCommand
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT lot_id
        FROM loyalty.point_lots
        WHERE status = 'ACTIVE'
          AND remaining_points > 0
          AND expires_at IS NOT NULL
          AND expires_at <= $1
        ORDER BY expires_at, lot_id
        LIMIT $2`, asOf, limit)
	if err != nil {
		return nil, fmt.Errorf("list expirable point lots: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expirable point lot: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expirable point lots: %w", err)
	}
	return ids, nil
}

func ParseEntryCursor(raw string) (*EntryCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, "|")
	if len(parts) != 2 {
		return nil, ErrInvalidCommand
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, ErrInvalidCommand
	}
	ledgerID, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, ErrInvalidCommand
	}
	return &EntryCursor{CreatedAt: createdAt, LedgerID: ledgerID}, nil
}

func FormatEntryCursor(cursor *EntryCursor) string {
	if cursor == nil {
		return ""
	}
	return cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + cursor.LedgerID.String()
}
