package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
	"github.com/google/uuid"
)

type ExpiryStore interface {
	ListExpiredReservationIDs(context.Context, time.Time, int) ([]uuid.UUID, error)
	ExpireReservation(context.Context, ledger.ReservationTransitionCommand) (ledger.ReservationResult, error)
	ListExpirableLotIDs(context.Context, time.Time, int) ([]uuid.UUID, error)
	ExpireLot(context.Context, uuid.UUID, string, time.Time) (ledger.Result, error)
}

type ExpiryWorker struct {
	store    ExpiryStore
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time
}

func NewExpiryWorker(store ExpiryStore, logger *slog.Logger) (*ExpiryWorker, error) {
	if store == nil || logger == nil {
		return nil, errors.New("expiry worker requires store and logger")
	}
	return &ExpiryWorker{store: store, logger: logger, interval: 30 * time.Second, now: time.Now}, nil
}

func (w *ExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("loyalty expiry cycle failed", "error_code", "expiry_cycle_failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *ExpiryWorker) RunOnce(ctx context.Context) error {
	asOf := w.now().UTC()
	var failures []error
	reservationIDs, err := w.store.ListExpiredReservationIDs(ctx, asOf, 100)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, reservationID := range reservationIDs {
			_, err := w.store.ExpireReservation(ctx, ledger.ReservationTransitionCommand{
				ReservationID: reservationID, IdempotencyKey: "reservation-expiry:" + reservationID.String(),
				OccurredAt: asOf, ActorType: ledger.ActorSystem,
			})
			if err != nil && !errors.Is(err, ledger.ErrInvalidTransition) {
				failures = append(failures, fmt.Errorf("expire reservation %s: %w", reservationID, err))
			}
		}
	}
	lotIDs, err := w.store.ListExpirableLotIDs(ctx, asOf, 100)
	if err != nil {
		failures = append(failures, err)
	} else {
		for _, lotID := range lotIDs {
			if _, err := w.store.ExpireLot(ctx, lotID, "lot-expiration:"+lotID.String(), asOf); err != nil &&
				!errors.Is(err, ledger.ErrInvalidTransition) {
				failures = append(failures, fmt.Errorf("expire lot %s: %w", lotID, err))
			}
		}
	}
	return errors.Join(failures...)
}
