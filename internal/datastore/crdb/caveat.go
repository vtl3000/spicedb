package crdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"

	pgxcommon "github.com/authzed/spicedb/internal/datastore/postgres/common"
	"github.com/authzed/spicedb/pkg/datastore"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
)

var (
	upsertCaveatSuffix = fmt.Sprintf(
		"ON CONFLICT (%s) DO UPDATE SET %s = excluded.%s",
		colCaveatName,
		colCaveatDefinition,
		colCaveatDefinition,
	)
	writeCaveat  = psql.Insert(tableCaveat).Columns(colCaveatName, colCaveatDefinition).Suffix(upsertCaveatSuffix)
	readCaveat   = psql.Select(colCaveatDefinition, colTimestamp)
	listCaveat   = psql.Select(colCaveatName, colCaveatDefinition, colTimestamp).From(tableCaveat).OrderBy(colCaveatName)
	deleteCaveat = psql.Delete(tableCaveat)
)

const (
	errWriteCaveat   = "unable to write new caveat revision: %w"
	errReadCaveat    = "unable to read new caveat `%s`: %w"
	errListCaveats   = "unable to list caveat: %w"
	errDeleteCaveats = "unable to delete caveats: %w"
)

func (cr *crdbReader) ReadCaveatByName(ctx context.Context, name string) (*core.CaveatDefinition, datastore.Revision, error) {
	query := cr.fromBuilder(readCaveat, tableCaveat).Where(sq.Eq{colCaveatName: name})
	sql, args, err := query.ToSql()
	if err != nil {
		return nil, datastore.NoRevision, fmt.Errorf(errReadCaveat, name, err)
	}

	var definitionBytes []byte
	var timestamp time.Time
	err = cr.executeWithTx(ctx, func(ctx context.Context, tx pgxcommon.DBReader) error {
		if err := tx.QueryRow(ctx, sql, args...).Scan(&definitionBytes, &timestamp); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				err = datastore.NewCaveatNameNotFoundErr(name)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, datastore.NoRevision, fmt.Errorf(errReadCaveat, name, err)
	}
	loaded := &core.CaveatDefinition{}
	if err := loaded.UnmarshalVT(definitionBytes); err != nil {
		return nil, datastore.NoRevision, fmt.Errorf(errReadCaveat, name, err)
	}
	cr.addOverlapKey(name)
	return loaded, revisionFromTimestamp(timestamp), nil
}

func (cr *crdbReader) LookupCaveatsWithNames(ctx context.Context, caveatNames []string) ([]datastore.RevisionedCaveat, error) {
	if len(caveatNames) == 0 {
		return nil, nil
	}
	return cr.lookupCaveats(ctx, caveatNames)
}

func (cr *crdbReader) ListAllCaveats(ctx context.Context) ([]datastore.RevisionedCaveat, error) {
	return cr.lookupCaveats(ctx, nil)
}

type bytesAndTimestamp struct {
	bytes     []byte
	timestamp time.Time
}

func (cr *crdbReader) lookupCaveats(ctx context.Context, caveatNames []string) ([]datastore.RevisionedCaveat, error) {
	caveatsWithNames := cr.fromBuilder(listCaveat, tableCaveat)
	if len(caveatNames) > 0 {
		caveatsWithNames = caveatsWithNames.Where(sq.Eq{colCaveatName: caveatNames})
	}

	sql, args, err := caveatsWithNames.ToSql()
	if err != nil {
		return nil, fmt.Errorf(errListCaveats, err)
	}
	var allDefinitionBytes []bytesAndTimestamp

	err = cr.executeWithTx(ctx, func(ctx context.Context, tx pgxcommon.DBReader) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var defBytes []byte
			var name string
			var timestamp time.Time
			err = rows.Scan(&name, &defBytes, &timestamp)
			if err != nil {
				return err
			}
			allDefinitionBytes = append(allDefinitionBytes, bytesAndTimestamp{bytes: defBytes, timestamp: timestamp})
			cr.addOverlapKey(name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf(errListCaveats, err)
	}

	caveats := make([]datastore.RevisionedCaveat, 0, len(allDefinitionBytes))
	for _, bat := range allDefinitionBytes {
		loaded := &core.CaveatDefinition{}
		if err := loaded.UnmarshalVT(bat.bytes); err != nil {
			return nil, fmt.Errorf(errListCaveats, err)
		}
		caveats = append(caveats, datastore.RevisionedCaveat{
			Definition:          loaded,
			LastWrittenRevision: revisionFromTimestamp(bat.timestamp),
		})
	}

	return caveats, nil
}

func (rwt *crdbReadWriteTXN) WriteCaveats(ctx context.Context, caveats []*core.CaveatDefinition) error {
	if len(caveats) == 0 {
		return nil
	}
	write := writeCaveat
	writtenCaveatNames := make([]string, 0, len(caveats))
	for _, caveat := range caveats {
		definitionBytes, err := caveat.MarshalVT()
		if err != nil {
			return fmt.Errorf(errWriteCaveat, err)
		}
		valuesToWrite := []any{caveat.Name, definitionBytes}
		write = write.Values(valuesToWrite...)
		writtenCaveatNames = append(writtenCaveatNames, caveat.Name)
	}

	// store the new caveat
	sql, args, err := write.ToSql()
	if err != nil {
		return fmt.Errorf(errWriteCaveat, err)
	}

	for _, val := range writtenCaveatNames {
		rwt.addOverlapKey(val)
	}
	return rwt.executeWithTx(ctx, func(ctx context.Context, tx pgxcommon.DBReader) error {
		if _, err := rwt.tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf(errWriteCaveat, err)
		}
		return nil
	})
}

func (rwt *crdbReadWriteTXN) DeleteCaveats(ctx context.Context, names []string) error {
	deleteCaveatClause := deleteCaveat.Where(sq.Eq{colCaveatName: names})
	sql, args, err := deleteCaveatClause.ToSql()
	if err != nil {
		return fmt.Errorf(errDeleteCaveats, err)
	}
	for _, val := range names {
		rwt.addOverlapKey(val)
	}
	return rwt.executeWithTx(ctx, func(ctx context.Context, tx pgxcommon.DBReader) error {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf(errDeleteCaveats, err)
		}
		return nil
	})
}

func (cr *crdbReader) executeWithTx(ctx context.Context, f func(ctx context.Context, tx pgxcommon.DBReader) error) error {
	return cr.execute(ctx, func(ctx context.Context) error {
		tx, txCleanup, err := cr.txSource(ctx)
		if err != nil {
			return err
		}
		defer txCleanup(ctx)

		return f(ctx, tx)
	})
}
