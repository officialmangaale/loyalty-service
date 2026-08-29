package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestScanIsReadOnlyBoundedAndReportsTruncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	detectedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	runID := uuid.New()
	store.now = func() time.Time { return detectedAt }
	store.newID = func() uuid.UUID { return runID }
	accountID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`WITH ledger_totals AS`).
		WithArgs(detectedAt, 3).
		WillReturnRows(findingRows().
			AddRow("ACCOUNT_LEDGER_PROJECTION_MISMATCH", "CRITICAL", accountID, "ACCOUNT", accountID.String(), []byte(`{"available_points":10}`), []byte(`{"available_points":9}`)).
			AddRow("EXPIRED_ACTIVE_RESERVATION", "WARNING", accountID, "RESERVATION", uuid.NewString(), []byte(`{"status":"RELEASED_OR_EXPIRED"}`), []byte(`{"status":"ACTIVE"}`)).
			AddRow("EXPIRED_ACTIVE_RESERVATION", "WARNING", accountID, "RESERVATION", uuid.NewString(), []byte(`{"status":"RELEASED_OR_EXPIRED"}`), []byte(`{"status":"ACTIVE"}`)))
	mock.ExpectCommit()

	report, err := store.Scan(context.Background(), 2)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Recorded || !report.Truncated || len(report.Findings) != 2 || report.RunID != runID {
		t.Fatalf("unexpected report: %#v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordWritesFindingAndImmutableRunAuditAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	detectedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	index := 0
	store.now = func() time.Time { return detectedAt }
	store.newID = func() uuid.UUID {
		value := ids[index]
		index++
		return value
	}
	accountID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WithArgs(lockID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`WITH ledger_totals AS`).
		WithArgs(detectedAt, 2).
		WillReturnRows(findingRows().AddRow(
			"ACCOUNT_LOT_PROJECTION_MISMATCH", "CRITICAL", accountID, "ACCOUNT", accountID.String(),
			[]byte(`{"available_points":10}`), []byte(`{"available_points":9}`),
		))
	mock.ExpectExec(`INSERT INTO loyalty.reconciliation_findings`).
		WithArgs(
			ids[1], "ACCOUNT_LOT_PROJECTION_MISMATCH", "CRITICAL", accountID,
			"ACCOUNT", accountID.String(), []byte(`{"available_points":10}`),
			[]byte(`{"available_points":9}`), detectedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO loyalty.audit_events`).
		WithArgs(ids[2], "operator@example", ids[0].String(), sqlmock.AnyArg(), detectedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	report, err := store.Record(context.Background(), 1, "operator@example")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !report.Recorded || report.Truncated || len(report.Findings) != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRequiresActorIdentityBeforeOpeningTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("open mock database: %v", err)
	}
	defer db.Close()
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(context.Background(), 10, "  "); err == nil {
		t.Fatal("blank actor identity was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func findingRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"finding_type", "severity", "account_id", "reference_type",
		"reference_id", "expected_state", "actual_state",
	})
}
