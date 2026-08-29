package migrations

import (
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedMigrationManifest(t *testing.T) {
	migrations, err := List()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(migrations) != 3 {
		t.Fatalf("migration count = %d, want 3", len(migrations))
	}

	previous := int64(0)
	checksumPattern := regexp.MustCompile(`^[a-f0-9]{64}$`)
	for _, migration := range migrations {
		if migration.Version <= previous {
			t.Fatalf("migration versions are not increasing: %d after %d", migration.Version, previous)
		}
		previous = migration.Version
		if !checksumPattern.MatchString(migration.UpChecksum) || !checksumPattern.MatchString(migration.DownChecksum) {
			t.Fatalf("migration %d has invalid checksum", migration.Version)
		}
		for direction, sql := range map[string]string{"up": migration.UpSQL, "down": migration.DownSQL} {
			upper := strings.ToUpper(sql)
			if strings.Contains(upper, "DROP DATABASE") || strings.Contains(upper, "TRUNCATE ") {
				t.Fatalf("migration %d %s contains prohibited destructive SQL", migration.Version, direction)
			}
		}
	}
}

func TestAccountingMigrationContainsSafetyInvariants(t *testing.T) {
	migrations, err := List()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	core := migrations[0]
	required := []string{
		"CREATE TABLE loyalty.ledger",
		"business_key text NOT NULL UNIQUE",
		"CHECK (available_points >= 0)",
		"ledger_reject_update_delete",
		"refusing core rollback",
	}
	for _, fragment := range required {
		if !strings.Contains(core.UpSQL+core.DownSQL, fragment) {
			t.Fatalf("core migration is missing %q", fragment)
		}
	}
}

func TestValidateCurrentRejectsMissingAndDriftedHistory(t *testing.T) {
	migrationList, err := List()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	statuses := make([]Status, len(migrationList))
	for index, migration := range migrationList {
		statuses[index] = Status{Version: migration.Version, Name: migration.Name, Checksum: migration.UpChecksum}
	}
	if err := ValidateCurrent(statuses); err != nil {
		t.Fatalf("valid migration history rejected: %v", err)
	}
	if err := ValidateCurrent(statuses[:len(statuses)-1]); err == nil {
		t.Fatal("missing migration was accepted")
	}
	drifted := append([]Status(nil), statuses...)
	drifted[0].Checksum = "drifted"
	if err := ValidateCurrent(drifted); err == nil {
		t.Fatal("checksum drift was accepted")
	}
}
