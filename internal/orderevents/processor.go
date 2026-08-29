package orderevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
)

type Ledger interface {
	RecordPendingEarn(context.Context, ledger.PendingEarnCommand) (ledger.Result, error)
	MakePendingAvailable(context.Context, ledger.PendingTransitionCommand) (ledger.Result, error)
	CancelPending(context.Context, ledger.PendingTransitionCommand) (ledger.Result, error)
}

type ProcessResult struct {
	Status   string
	Replayed bool
}

type Processor struct {
	db             *sql.DB
	ledger         Ledger
	earningEnabled bool
	now            func() time.Time
}

func NewProcessor(db *sql.DB, ledgerStore Ledger, earningEnabled ...bool) (*Processor, error) {
	if db == nil || ledgerStore == nil {
		return nil, errors.New("order event processor requires database and ledger")
	}
	enabled := false
	if len(earningEnabled) > 0 {
		enabled = earningEnabled[0]
	}
	return &Processor{db: db, ledger: ledgerStore, earningEnabled: enabled, now: time.Now}, nil
}

func (p *Processor) Process(ctx context.Context, event Envelope, rawPayload []byte) (ProcessResult, error) {
	if err := event.Validate(p.now().UTC()); err != nil {
		return ProcessResult{}, err
	}
	if len(rawPayload) == 0 || len(rawPayload) > 64*1024 || !json.Valid(rawPayload) {
		return ProcessResult{}, ErrInvalidEvent
	}
	status, replayed, err := p.accept(ctx, event, rawPayload)
	if err != nil {
		return ProcessResult{}, err
	}
	if status == "PROCESSED" {
		return ProcessResult{Status: status, Replayed: true}, nil
	}
	ready, err := p.predecessorProcessed(ctx, event)
	if err != nil {
		return ProcessResult{}, p.fail(ctx, event, "sequence_check_failed", err)
	}
	if !ready {
		if err := p.setStatus(ctx, event, "HELD", "waiting_for_predecessor"); err != nil {
			return ProcessResult{}, err
		}
		return ProcessResult{Status: "HELD", Replayed: replayed}, nil
	}
	if err := p.setStatus(ctx, event, "PROCESSING", ""); err != nil {
		return ProcessResult{}, err
	}
	if err := p.apply(ctx, event); err != nil {
		if errors.Is(err, ErrNoActivePolicy) || errors.Is(err, ErrEarningDisabled) {
			code := "earning_policy_unavailable"
			if errors.Is(err, ErrEarningDisabled) {
				code = "earning_globally_disabled"
			}
			if statusErr := p.setStatus(ctx, event, "HELD", code); statusErr != nil {
				return ProcessResult{}, statusErr
			}
			return ProcessResult{Status: "HELD", Replayed: replayed}, nil
		}
		return ProcessResult{}, p.fail(ctx, event, "effect_failed", err)
	}
	if err := p.markProcessed(ctx, event); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{Status: "PROCESSED", Replayed: replayed}, nil
}

func (p *Processor) apply(ctx context.Context, event Envelope) error {
	switch event.EventType {
	case EventOrderPlaced:
		if !p.earningEnabled {
			return ErrEarningDisabled
		}
		userID, linked := event.PlatformUserUUID()
		if !linked {
			return nil
		}
		enabled, err := p.earningEntitled(ctx, event.Data.RestaurantID)
		if err != nil || !enabled {
			return err
		}
		policy, err := p.activeEarningPolicy(ctx, event.OccurredAt)
		if err != nil {
			return err
		}
		points, expiresAt, eligible, err := policy.Evaluate(event)
		if err != nil || !eligible {
			return err
		}
		restaurantID := event.Data.RestaurantID
		metadata, _ := json.Marshal(map[string]any{
			"event_id": event.EventID.String(), "basis": policy.Basis,
			"amount_millis_denominator": policy.AmountMillisDenominator,
			"points_numerator":          policy.PointsNumerator, "expiry_anchor": policy.ExpiryAnchor,
		})
		_, err = p.ledger.RecordPendingEarn(ctx, ledger.PendingEarnCommand{
			PlatformUserID: userID, OrderID: event.AggregateID, EffectKey: "base",
			Points: points, RestaurantID: &restaurantID,
			FundingSource: ledger.FundingSource(policy.FundingSource), PolicyVersion: policy.Version,
			IdempotencyKey: event.EventID.String(), ExpiresAt: expiresAt,
			OccurredAt: event.OccurredAt, Metadata: metadata,
		})
		return err
	case EventOrderCompleted:
		_, err := p.ledger.MakePendingAvailable(ctx, ledger.PendingTransitionCommand{
			PendingBusinessKey: pendingBusinessKey(event.AggregateID),
			IdempotencyKey:     event.EventID.String(), OccurredAt: event.OccurredAt,
			ActorType: ledger.ActorSystem, Metadata: eventMetadata(event),
		})
		if errors.Is(err, ledger.ErrNotFound) {
			return nil
		}
		return err
	case EventOrderCancelled, EventOrderRejected:
		_, err := p.ledger.CancelPending(ctx, ledger.PendingTransitionCommand{
			PendingBusinessKey: pendingBusinessKey(event.AggregateID),
			IdempotencyKey:     event.EventID.String(), OccurredAt: event.OccurredAt,
			ActorType: ledger.ActorSystem, Metadata: eventMetadata(event),
		})
		if errors.Is(err, ledger.ErrNotFound) {
			return nil
		}
		return err
	default:
		return ErrUnsupportedEvent
	}
}

func (p *Processor) ReplayHeld(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT payload
		FROM loyalty.inbox_events
		WHERE status = 'HELD'
		  AND COALESCE(last_error_at, received_at) <= CURRENT_TIMESTAMP - make_interval(secs => 30)
		ORDER BY received_at, event_id
		LIMIT $1`, limit)
	if err != nil {
		return fmt.Errorf("list held loyalty events: %w", err)
	}
	defer rows.Close()
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		payloads = append(payloads, append([]byte(nil), payload...))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, payload := range payloads {
		event, err := DecodeStrict(payload)
		if err != nil {
			continue
		}
		if _, err := p.Process(ctx, event, payload); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) accept(ctx context.Context, event Envelope, payload []byte) (string, bool, error) {
	result, err := p.db.ExecContext(ctx, `
		INSERT INTO loyalty.inbox_events (
			event_id, event_type, event_version, aggregate_type,
			aggregate_id, aggregate_sequence, payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`, event.EventID, event.EventType, event.SchemaVersion,
		event.AggregateType, event.AggregateID, event.Sequence, payload)
	if err != nil {
		return "", false, fmt.Errorf("persist loyalty inbox event: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if affected == 1 {
		return "RECEIVED", false, nil
	}
	var status, eventType, aggregateID string
	var sequence int64
	var payloadMatches bool
	err = p.db.QueryRowContext(ctx, `
		SELECT status, event_type, aggregate_id, aggregate_sequence, payload = $2::jsonb
		FROM loyalty.inbox_events WHERE event_id = $1`, event.EventID, payload).
		Scan(&status, &eventType, &aggregateID, &sequence, &payloadMatches)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrEventConflict
	}
	if err != nil {
		return "", false, err
	}
	if eventType != event.EventType || aggregateID != event.AggregateID || sequence != event.Sequence || !payloadMatches {
		return "", false, ErrEventConflict
	}
	return status, true, nil
}

func (p *Processor) predecessorProcessed(ctx context.Context, event Envelope) (bool, error) {
	if event.Sequence == 1 {
		return true, nil
	}
	var exists bool
	err := p.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM loyalty.inbox_events
			WHERE aggregate_type = $1 AND aggregate_id = $2
			  AND aggregate_sequence = $3 AND status = 'PROCESSED'
		)`, event.AggregateType, event.AggregateID, event.Sequence-1).Scan(&exists)
	return exists, err
}

func (p *Processor) earningEntitled(ctx context.Context, restaurantID int64) (bool, error) {
	var enabled bool
	err := p.db.QueryRowContext(ctx, `
		SELECT COALESCE((
			SELECT enabled FROM loyalty.entitlements
			WHERE feature_key = 'ORDER_EARNING' AND cohort_key = 'ALL'
			  AND (restaurant_id = $1 OR restaurant_id IS NULL)
			ORDER BY CASE WHEN restaurant_id = $1 THEN 0 ELSE 1 END
			LIMIT 1
		), false)`, restaurantID).Scan(&enabled)
	return enabled, err
}

func (p *Processor) activeEarningPolicy(ctx context.Context, occurredAt time.Time) (EarningPolicy, error) {
	var version string
	var configuration []byte
	err := p.db.QueryRowContext(ctx, `
		SELECT version, configuration
		FROM loyalty.policies
		WHERE policy_type = $1 AND status = 'ACTIVE'
		  AND (effective_from IS NULL OR effective_from <= $2)
		  AND (effective_until IS NULL OR effective_until > $2)
		LIMIT 1`, policyTypeOrderEarning, occurredAt).Scan(&version, &configuration)
	if errors.Is(err, sql.ErrNoRows) {
		return EarningPolicy{}, ErrNoActivePolicy
	}
	if err != nil {
		return EarningPolicy{}, err
	}
	return decodePolicy(version, configuration)
}

func (p *Processor) setStatus(ctx context.Context, event Envelope, status, errorCode string) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE loyalty.inbox_events
		SET status = $2, attempts = attempts + CASE WHEN $2 = 'PROCESSING' THEN 1 ELSE 0 END,
		    last_error_code = NULLIF($3, ''),
		    last_error_at = CASE WHEN $3 = '' THEN NULL ELSE CURRENT_TIMESTAMP END
		WHERE event_id = $1`, event.EventID, status, errorCode)
	return err
}

func (p *Processor) markProcessed(ctx context.Context, event Envelope) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE loyalty.inbox_events
		SET status = 'PROCESSED', processed_at = CURRENT_TIMESTAMP,
		    last_error_code = NULL, last_error_at = NULL
		WHERE event_id = $1`, event.EventID)
	return err
}

func (p *Processor) fail(ctx context.Context, event Envelope, code string, cause error) error {
	_ = p.setStatus(ctx, event, "FAILED", code)
	return cause
}

func pendingBusinessKey(orderID string) string {
	return "order:" + orderID + ":earn:pending:base"
}

func eventMetadata(event Envelope) json.RawMessage {
	metadata, _ := json.Marshal(map[string]string{
		"event_id": event.EventID.String(), "event_type": event.EventType,
		"previous_status": strings.ToLower(event.Data.PreviousStatus),
	})
	return metadata
}
