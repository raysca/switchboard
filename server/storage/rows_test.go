package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

type fakeResult struct {
	n   int64
	err error
}

func (f fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (f fakeResult) RowsAffected() (int64, error) { return f.n, f.err }

func scanInt(r rowScanner) (int, error) {
	var v int
	err := r.Scan(&v)
	return v, err
}

// queryRows runs query on the store's pool and fails the test on error.
func queryRows(t *testing.T, s *Store, query string) *sql.Rows {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertNoConnInUse(t *testing.T, s *Store) {
	t.Helper()
	if n := s.DB().Stats().InUse; n != 0 {
		t.Fatalf("connections in use = %d, rows were not closed", n)
	}
}

func TestCollectKeepsStartingSlice(t *testing.T) {
	s := testStore(t)
	const empty = "SELECT 1 WHERE 1=0"

	got, err := collect(queryRows(t, s, empty), nil, scanInt)
	if err != nil || got != nil {
		t.Fatalf("nil start: got %#v, %v; want nil, nil", got, err)
	}
	got, err = collect(queryRows(t, s, empty), make([]int, 0, 4), scanInt)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("make start: got %#v, %v; want non-nil empty", got, err)
	}
	got, err = collect(queryRows(t, s, "SELECT 1 UNION ALL SELECT 2 ORDER BY 1"), []int{0}, scanInt)
	if err != nil || len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("appends: got %v, %v", got, err)
	}
	assertNoConnInUse(t, s)
}

func TestCollectScanErrorClosesRows(t *testing.T) {
	s := testStore(t)
	boom := errors.New("boom")
	calls := 0
	got, err := collect(queryRows(t, s, "SELECT 1 UNION ALL SELECT 2"), nil, func(rowScanner) (int, error) {
		calls++
		return 0, boom
	})
	if !errors.Is(err, boom) || got != nil || calls != 1 {
		t.Fatalf("got %v, %v after %d calls; want nil, boom after 1", got, err, calls)
	}
	assertNoConnInUse(t, s)
}

func TestOneRow(t *testing.T) {
	execErr, affErr := errors.New("exec"), errors.New("rows affected")
	for _, tc := range []struct {
		name string
		res  sql.Result
		err  error
		want error
	}{
		{"exec error", nil, execErr, execErr},
		{"rows affected error", fakeResult{err: affErr}, nil, affErr},
		{"no row", fakeResult{n: 0}, nil, sql.ErrNoRows},
		{"one row", fakeResult{n: 1}, nil, nil},
		{"two rows", fakeResult{n: 2}, nil, sql.ErrNoRows},
	} {
		if got := oneRow(tc.res, tc.err); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQueryIDs(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	const q = "SELECT 'b' UNION ALL SELECT 'a' ORDER BY 1"

	ids, err := queryIDs(ctx, s.DB(), q)
	if err != nil || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("db: got %v, %v", ids, err)
	}
	assertNoConnInUse(t, s)
	if ids, err := queryIDs(ctx, s.DB(), "SELECT 'a' WHERE 1=0"); err != nil || ids != nil {
		t.Fatalf("empty: got %#v, %v; want nil, nil", ids, err)
	}
	if _, err := queryIDs(ctx, s.DB(), "SELECT id FROM no_such_table"); err == nil {
		t.Fatal("query error not returned")
	}

	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		ids, err := queryIDs(ctx, tx, s.bind("SELECT ? UNION ALL SELECT ?"), "x", "y")
		if err != nil {
			return err
		}
		if len(ids) != 2 {
			t.Errorf("tx: got %v", ids)
		}
		// The rows are closed, so the transaction can run its next statement.
		var n int
		return tx.QueryRowContext(ctx, "SELECT count(*) FROM agents").Scan(&n)
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	assertNoConnInUse(t, s)
}

// An error while stepping (SQLite's abs(minint) overflows on the second row) reaches the caller with the rows
// read so far, and the rows are closed.
func TestCollectReturnsRowsErr(t *testing.T) {
	s := testStore(t)
	got, err := collect(queryRows(t, s, "SELECT 1 UNION ALL SELECT abs(-9223372036854775808)"), nil, scanInt)
	if err == nil {
		t.Fatalf("got %v, nil; want the step error", got)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("got %v; want the first row", got)
	}
	assertNoConnInUse(t, s)
}
