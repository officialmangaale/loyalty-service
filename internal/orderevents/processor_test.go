package orderevents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/internal/ledger"
)

type fakeLedger struct {
	pendingCommand ledger.PendingEarnCommand
	pendingCalls   int
}

func (f *fakeLedger) RecordPendingEarn(_ context.Context, command ledger.PendingEarnCommand) (ledger.Result, error) {
	f.pendingCalls++
	f.pendingCommand = command
	return ledger.Result{}, nil
}

func (f *fakeLedger) MakePendingAvailable(context.Context, ledger.PendingTransitionCommand) (ledger.Result, error) {
	return ledger.Result{}, nil
}

func (f *fakeLedger) CancelPending(context.Context, ledger.PendingTransitionCommand) (ledger.Result, error) {
	return ledger.Result{}, nil
}

func TestProcessorPersistsThenAppliesApprovedEntitledEarning(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()
	event := validPlacedEvent()
	payload, _ := json.Marshal(event)
	mock.ExpectExec(`INSERT INTO loyalty\.inbox_events`).
		WithArgs(event.EventID, event.EventType, event.SchemaVersion, event.AggregateType, event.AggregateID, event.Sequence, payload).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE loyalty\.inbox_events`).
		WithArgs(event.EventID, "PROCESSING", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COALESCE`).WithArgs(event.Data.RestaurantID).
		WillReturnRows(sqlmock.NewRows([]string{"enabled"}).AddRow(true))
	policy := `{"basis":"PAYABLE","points_numerator":1,"amount_millis_denominator":1000,"expiry_days":180,"expiry_anchor":"ORDER_PLACED","allowed_order_types":["DELIVERY"],"allow_qr":true,"funding_source":"MANGAALE"}`
	mock.ExpectQuery(`SELECT version, configuration`).WithArgs(policyTypeOrderEarning, event.OccurredAt).
		WillReturnRows(sqlmock.NewRows([]string{"version", "configuration"}).AddRow("earn-v1", []byte(policy)))
	mock.ExpectExec(`UPDATE loyalty\.inbox_events`).WithArgs(event.EventID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ledgerStore := &fakeLedger{}
	processor, err := NewProcessor(db, ledgerStore, true)
	if err != nil {
		t.Fatalf("create processor: %v", err)
	}
	result, err := processor.Process(context.Background(), event, payload)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if result.Status != "PROCESSED" || ledgerStore.pendingCalls != 1 {
		t.Fatalf("result=%#v pending calls=%d", result, ledgerStore.pendingCalls)
	}
	if ledgerStore.pendingCommand.Points != 500 || ledgerStore.pendingCommand.PolicyVersion != "earn-v1" ||
		ledgerStore.pendingCommand.IdempotencyKey != event.EventID.String() {
		t.Fatalf("unexpected pending command: %#v", ledgerStore.pendingCommand)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessorHoldsOutOfOrderEventWithoutLedgerEffect(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()
	event := validPlacedEvent()
	event.EventType = EventOrderCompleted
	event.Data.OrderStatus = "completed"
	event.Sequence = 2
	payload, _ := json.Marshal(event)
	mock.ExpectExec(`INSERT INTO loyalty\.inbox_events`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(event.AggregateType, event.AggregateID, int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(`UPDATE loyalty\.inbox_events`).
		WithArgs(event.EventID, "HELD", "waiting_for_predecessor").
		WillReturnResult(sqlmock.NewResult(0, 1))

	ledgerStore := &fakeLedger{}
	processor, _ := NewProcessor(db, ledgerStore)
	result, err := processor.Process(context.Background(), event, payload)
	if err != nil || result.Status != "HELD" || ledgerStore.pendingCalls != 0 {
		t.Fatalf("result=%#v err=%v pending calls=%d", result, err, ledgerStore.pendingCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessorRejectsMutatedReplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()
	event := validPlacedEvent()
	payload, _ := json.Marshal(event)
	mock.ExpectExec(`INSERT INTO loyalty\.inbox_events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT status, event_type`).
		WithArgs(event.EventID, payload).
		WillReturnRows(sqlmock.NewRows([]string{"status", "event_type", "aggregate_id", "aggregate_sequence", "payload_matches"}).
			AddRow("PROCESSED", event.EventType, event.AggregateID, event.Sequence, false))
	processor, _ := NewProcessor(db, &fakeLedger{})
	_, err = processor.Process(context.Background(), event, payload)
	if !errors.Is(err, ErrEventConflict) {
		t.Fatalf("mutated replay error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
