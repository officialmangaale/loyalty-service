package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	defaultLimit = 100
	maximumLimit = 1000
	lockID       = int64(517_611_908_2027)
)

type Finding struct {
	Type          string          `json:"type"`
	Severity      string          `json:"severity"`
	AccountID     *uuid.UUID      `json:"account_id,omitempty"`
	ReferenceType string          `json:"reference_type"`
	ReferenceID   string          `json:"reference_id"`
	ExpectedState json.RawMessage `json:"expected_state"`
	ActualState   json.RawMessage `json:"actual_state"`
}

type Report struct {
	RunID      uuid.UUID `json:"run_id"`
	DetectedAt time.Time `json:"detected_at"`
	Recorded   bool      `json:"recorded"`
	Truncated  bool      `json:"truncated"`
	Findings   []Finding `json:"findings"`
}

type Store struct {
	db    *sql.DB
	now   func() time.Time
	newID func() uuid.UUID
}

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("reconciliation database is required")
	}
	return &Store{db: db, now: time.Now, newID: uuid.New}, nil
}

func (s *Store) Scan(ctx context.Context, limit int) (Report, error) {
	limit = boundedLimit(limit)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Report{}, fmt.Errorf("begin read-only reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	report, err := s.scan(ctx, tx, limit)
	if err != nil {
		return Report{}, err
	}
	if err := tx.Commit(); err != nil {
		return Report{}, fmt.Errorf("commit read-only reconciliation: %w", err)
	}
	return report, nil
}

func (s *Store) Record(ctx context.Context, limit int, actorID string) (Report, error) {
	actorID = strings.TrimSpace(actorID)
	if actorID == "" || len(actorID) > 200 {
		return Report{}, errors.New("reconciliation actor ID must contain 1 to 200 characters")
	}
	limit = boundedLimit(limit)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Report{}, fmt.Errorf("begin recorded reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		return Report{}, fmt.Errorf("lock reconciliation run: %w", err)
	}
	report, err := s.scan(ctx, tx, limit)
	if err != nil {
		return Report{}, err
	}
	for _, finding := range report.Findings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO loyalty.reconciliation_findings (
				finding_id, finding_type, severity, account_id, reference_type,
				reference_id, expected_state, actual_state, detected_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (finding_type, reference_type, reference_id, status)
			DO UPDATE SET
				severity = EXCLUDED.severity,
				account_id = EXCLUDED.account_id,
				expected_state = EXCLUDED.expected_state,
				actual_state = EXCLUDED.actual_state,
				detected_at = EXCLUDED.detected_at`,
			s.newID(), finding.Type, finding.Severity, nullableUUID(finding.AccountID),
			finding.ReferenceType, finding.ReferenceID, finding.ExpectedState,
			finding.ActualState, report.DetectedAt); err != nil {
			return Report{}, fmt.Errorf("record reconciliation finding: %w", err)
		}
	}
	summary, err := json.Marshal(map[string]any{
		"finding_count": len(report.Findings),
		"truncated":     report.Truncated,
	})
	if err != nil {
		return Report{}, fmt.Errorf("encode reconciliation summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO loyalty.audit_events (
			audit_id, event_type, actor_type, actor_id, subject_type,
			subject_id, reason, after_state, metadata, occurred_at
		) VALUES ($1, 'RECONCILIATION_RECORDED', 'RECONCILER', $2,
			'RECONCILIATION_RUN', $3, 'bounded invariant scan', $4, '{}'::jsonb, $5)`,
		s.newID(), actorID, report.RunID.String(), summary, report.DetectedAt); err != nil {
		return Report{}, fmt.Errorf("audit reconciliation run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Report{}, fmt.Errorf("commit recorded reconciliation: %w", err)
	}
	report.Recorded = true
	return report, nil
}

func (s *Store) scan(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, limit int) (Report, error) {
	detectedAt := s.now().UTC()
	runID := s.newID()
	rows, err := queryer.QueryContext(ctx, reconciliationQuery, detectedAt, limit+1)
	if err != nil {
		return Report{}, fmt.Errorf("query reconciliation findings: %w", err)
	}
	defer rows.Close()
	findings := make([]Finding, 0, limit)
	truncated := false
	for rows.Next() {
		if len(findings) == limit {
			truncated = true
			break
		}
		var finding Finding
		var accountID uuid.NullUUID
		if err := rows.Scan(
			&finding.Type, &finding.Severity, &accountID, &finding.ReferenceType,
			&finding.ReferenceID, &finding.ExpectedState, &finding.ActualState,
		); err != nil {
			return Report{}, fmt.Errorf("scan reconciliation finding: %w", err)
		}
		if accountID.Valid {
			value := accountID.UUID
			finding.AccountID = &value
		}
		if !json.Valid(finding.ExpectedState) || !json.Valid(finding.ActualState) {
			return Report{}, errors.New("reconciliation query returned invalid JSON state")
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("iterate reconciliation findings: %w", err)
	}
	return Report{RunID: runID, DetectedAt: detectedAt, Truncated: truncated, Findings: findings}, nil
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maximumLimit {
		return maximumLimit
	}
	return limit
}

func nullableUUID(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return *value
}

const reconciliationQuery = `
WITH ledger_totals AS (
	SELECT account_id,
	       COALESCE(SUM(available_delta), 0)::bigint AS available_points,
	       COALESCE(SUM(pending_delta), 0)::bigint AS pending_points,
	       COALESCE(SUM(reserved_delta), 0)::bigint AS reserved_points,
	       COALESCE(SUM(debt_delta), 0)::bigint AS debt_points
	FROM loyalty.ledger
	GROUP BY account_id
), lot_totals AS (
	SELECT account_id,
	       COALESCE(SUM(remaining_points), 0)::bigint AS available_points,
	       COALESCE(SUM(reserved_points), 0)::bigint AS reserved_points
	FROM loyalty.point_lots
	GROUP BY account_id
), active_reservations AS (
	SELECT account_id, COALESCE(SUM(points), 0)::bigint AS reserved_points
	FROM loyalty.reservations
	WHERE status = 'ACTIVE'
	GROUP BY account_id
), findings AS (
	SELECT 'ACCOUNT_LEDGER_PROJECTION_MISMATCH'::text AS finding_type,
	       'CRITICAL'::text AS severity, account.account_id,
	       'ACCOUNT'::text AS reference_type, account.account_id::text AS reference_id,
	       jsonb_build_object(
	           'available_points', COALESCE(ledger.available_points, 0),
	           'pending_points', COALESCE(ledger.pending_points, 0),
	           'reserved_points', COALESCE(ledger.reserved_points, 0),
	           'debt_points', COALESCE(ledger.debt_points, 0)
	       ) AS expected_state,
	       jsonb_build_object(
	           'available_points', account.available_points,
	           'pending_points', account.pending_points,
	           'reserved_points', account.reserved_points,
	           'debt_points', account.debt_points
	       ) AS actual_state
	FROM loyalty.accounts AS account
	LEFT JOIN ledger_totals AS ledger ON ledger.account_id = account.account_id
	WHERE (account.available_points, account.pending_points, account.reserved_points, account.debt_points)
	   IS DISTINCT FROM (
	       COALESCE(ledger.available_points, 0), COALESCE(ledger.pending_points, 0),
	       COALESCE(ledger.reserved_points, 0), COALESCE(ledger.debt_points, 0)
	   )

	UNION ALL

	SELECT 'ACCOUNT_LOT_PROJECTION_MISMATCH', 'CRITICAL', account.account_id,
	       'ACCOUNT', account.account_id::text,
	       jsonb_build_object(
	           'available_points', COALESCE(lot.available_points, 0),
	           'reserved_points', COALESCE(lot.reserved_points, 0)
	       ),
	       jsonb_build_object(
	           'available_points', account.available_points,
	           'reserved_points', account.reserved_points
	       )
	FROM loyalty.accounts AS account
	LEFT JOIN lot_totals AS lot ON lot.account_id = account.account_id
	WHERE (account.available_points, account.reserved_points)
	   IS DISTINCT FROM (COALESCE(lot.available_points, 0), COALESCE(lot.reserved_points, 0))

	UNION ALL

	SELECT 'ACTIVE_RESERVATION_PROJECTION_MISMATCH', 'CRITICAL', account.account_id,
	       'ACCOUNT', account.account_id::text,
	       jsonb_build_object('reserved_points', COALESCE(reservation.reserved_points, 0)),
	       jsonb_build_object('reserved_points', account.reserved_points)
	FROM loyalty.accounts AS account
	LEFT JOIN active_reservations AS reservation ON reservation.account_id = account.account_id
	WHERE account.reserved_points IS DISTINCT FROM COALESCE(reservation.reserved_points, 0)

	UNION ALL

	SELECT 'RESERVATION_ALLOCATION_MISMATCH', 'CRITICAL', reservation.account_id,
	       'RESERVATION', reservation.reservation_id::text,
	       jsonb_build_object('points', reservation.points),
	       jsonb_build_object('allocated_points', COALESCE(SUM(allocation.points), 0))
	FROM loyalty.reservations AS reservation
	LEFT JOIN loyalty.reservation_lot_allocations AS allocation
	       ON allocation.reservation_id = reservation.reservation_id
	WHERE reservation.status = 'ACTIVE'
	GROUP BY reservation.reservation_id
	HAVING reservation.points IS DISTINCT FROM COALESCE(SUM(allocation.points), 0)

	UNION ALL

	SELECT 'EXPIRED_ACTIVE_RESERVATION', 'WARNING', reservation.account_id,
	       'RESERVATION', reservation.reservation_id::text,
	       jsonb_build_object('status', 'RELEASED_OR_EXPIRED'),
	       jsonb_build_object('status', reservation.status, 'expires_at', reservation.expires_at)
	FROM loyalty.reservations AS reservation
	WHERE reservation.status = 'ACTIVE' AND reservation.expires_at <= $1

	UNION ALL

	SELECT 'REWARD_REDEMPTION_STATE_MISMATCH', 'CRITICAL', redemption.account_id,
	       'REWARD_REDEMPTION', redemption.redemption_id::text,
	       jsonb_build_object('status', CASE reservation.status
	           WHEN 'ACTIVE' THEN 'RESERVED'
	           WHEN 'CAPTURED' THEN 'CAPTURED'
	           ELSE 'RELEASED'
	       END),
	       jsonb_build_object('status', redemption.status, 'reservation_status', reservation.status)
	FROM loyalty.reward_redemptions AS redemption
	JOIN loyalty.reservations AS reservation
	  ON reservation.reservation_id = redemption.reservation_id
	WHERE redemption.status IS DISTINCT FROM CASE reservation.status
	          WHEN 'ACTIVE' THEN 'RESERVED'
	          WHEN 'CAPTURED' THEN 'CAPTURED'
	          ELSE 'RELEASED'
	      END
)
SELECT finding_type, severity, account_id, reference_type, reference_id,
       expected_state, actual_state
FROM findings
ORDER BY CASE severity WHEN 'CRITICAL' THEN 0 WHEN 'WARNING' THEN 1 ELSE 2 END,
         finding_type, reference_id
LIMIT $2`
