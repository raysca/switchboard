package storage

import (
	"context"
	"database/sql"
)

// collect scans every row with scan, appends to out and closes rows. Callers pass the slice they start with
// (nil or make(..., 0, n)), so an empty result stays nil or empty as before. A scan error returns nil.
func collect[T any](rows *sql.Rows, out []T, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// oneRow is the result check of an UPDATE or DELETE of one row by key: err, then the RowsAffected error, then
// sql.ErrNoRows unless exactly one row changed.
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// queryIDs runs a query that selects one string column and returns its values; rows are closed on every path,
// so a transaction can run its next statement.
func queryIDs(ctx context.Context, q rowQuerier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, nil, func(r rowScanner) (string, error) {
		var id string
		err := r.Scan(&id)
		return id, err
	})
}
