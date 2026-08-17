package alkemiodb

import (
	"context"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
)

func TestCIDMigrationAdapter_ListCandidates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	cid := "Qmaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT "externalID", COUNT(*) AS reference_count
FROM file
WHERE "externalID" ~ '^Qm[1-9A-HJ-NP-Za-km-z]{44}$'
GROUP BY "externalID"
ORDER BY "externalID"`)).
		WillReturnRows(mock.NewRows([]string{"externalID", "reference_count"}).AddRow(cid, int64(4)))

	rows, err := New(mock).ListCIDCandidates(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ExternalID != cid || rows[0].ReferenceCount != 4 {
		t.Fatalf("ListCIDCandidates = (%+v, %v)", rows, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCIDMigrationAdapter_ListCaseAliases(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	upper := "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	mock.ExpectQuery(`(?s)SELECT "externalID", COUNT\(\*\).*"externalID" <> lower\("externalID"\)`).
		WillReturnRows(mock.NewRows([]string{"externalID", "reference_count"}).AddRow(upper, int64(2)))

	rows, err := New(mock).ListCIDCaseAliases(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ExternalID != upper || rows[0].ReferenceCount != 2 {
		t.Fatalf("ListCIDCaseAliases = (%+v, %v)", rows, err)
	}
}

func TestCIDMigrationAdapter_UpdateGroupAndCount(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	target := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	aliases := []string{"Qmaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE file
SET "externalID" = $1
WHERE "externalID" = ANY($2::text[])
  AND "externalID" <> $1`)).
		WithArgs(target, aliases).
		WillReturnResult(pgxmock.NewResult("UPDATE", 6))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*)
FROM file
WHERE "externalID" = $1`)).
		WithArgs(aliases[0]).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(int64(0)))

	a := New(mock)
	changed, err := a.UpdateCIDGroup(context.Background(), target, aliases)
	if err != nil || changed != 6 {
		t.Fatalf("UpdateCIDGroup = (%d, %v)", changed, err)
	}
	count, err := a.CountCIDAliasReferences(context.Background(), aliases[0])
	if err != nil || count != 0 {
		t.Fatalf("CountCIDAliasReferences = (%d, %v)", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
