package postgres

import (
	"context"
	"errors"
	"fmt"

	sq "github.com/Masterminds/squirrel"

	"github.com/authzed/spicedb/internal/datastore/common"
	pgxcommon "github.com/authzed/spicedb/internal/datastore/postgres/common"
	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/datastore/options"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
)

type pgReader struct {
	txSource      pgxcommon.TxFactory
	querySplitter common.TupleQuerySplitter
	filterer      queryFilterer
}

type queryFilterer func(original sq.SelectBuilder) sq.SelectBuilder

var (
	queryTuples = psql.Select(
		colNamespace,
		colObjectID,
		colRelation,
		colUsersetNamespace,
		colUsersetObjectID,
		colUsersetRelation,
		colCaveatContextName,
		colCaveatContext,
	).From(tableTuple)

	schema = common.NewSchemaInformation(
		colNamespace,
		colObjectID,
		colRelation,
		colUsersetNamespace,
		colUsersetObjectID,
		colUsersetRelation,
		colCaveatContextName,
		common.TupleComparison,
	)

	readNamespace = psql.
			Select(colConfig, colCreatedXid).
			From(tableNamespace)
)

const (
	errUnableToReadConfig     = "unable to read namespace config: %w"
	errUnableToListNamespaces = "unable to list namespaces: %w"
)

func (r *pgReader) QueryRelationships(
	ctx context.Context,
	filter datastore.RelationshipsFilter,
	opts ...options.QueryOptionsOption,
) (iter datastore.RelationshipIterator, err error) {
	qBuilder, err := common.NewSchemaQueryFilterer(schema, r.filterer(queryTuples)).FilterWithRelationshipsFilter(filter)
	if err != nil {
		return nil, err
	}

	return r.querySplitter.SplitAndExecuteQuery(ctx, qBuilder, opts...)
}

func (r *pgReader) ReverseQueryRelationships(
	ctx context.Context,
	subjectsFilter datastore.SubjectsFilter,
	opts ...options.ReverseQueryOptionsOption,
) (iter datastore.RelationshipIterator, err error) {
	qBuilder, err := common.NewSchemaQueryFilterer(schema, r.filterer(queryTuples)).
		FilterWithSubjectsSelectors(subjectsFilter.AsSelector())
	if err != nil {
		return nil, err
	}

	queryOpts := options.NewReverseQueryOptionsWithOptions(opts...)

	if queryOpts.ResRelation != nil {
		qBuilder = qBuilder.
			FilterToResourceType(queryOpts.ResRelation.Namespace).
			FilterToRelation(queryOpts.ResRelation.Relation)
	}

	return r.querySplitter.SplitAndExecuteQuery(ctx,
		qBuilder,
		options.WithLimit(queryOpts.LimitForReverse),
		options.WithAfter(queryOpts.AfterForReverse),
		options.WithSort(queryOpts.SortForReverse),
	)
}

func (r *pgReader) ReadNamespaceByName(ctx context.Context, nsName string) (*core.NamespaceDefinition, datastore.Revision, error) {
	tx, txCleanup, err := r.txSource(ctx)
	if err != nil {
		return nil, datastore.NoRevision, fmt.Errorf(errUnableToReadConfig, err)
	}
	defer txCleanup(ctx)

	loaded, version, err := r.loadNamespace(ctx, nsName, tx, r.filterer)
	switch {
	case errors.As(err, &datastore.ErrNamespaceNotFound{}):
		return nil, datastore.NoRevision, err
	case err == nil:
		return loaded, version, nil
	default:
		return nil, datastore.NoRevision, fmt.Errorf(errUnableToReadConfig, err)
	}
}

func (r *pgReader) loadNamespace(ctx context.Context, namespace string, tx pgxcommon.DBReader, filterer queryFilterer) (*core.NamespaceDefinition, postgresRevision, error) {
	ctx, span := tracer.Start(ctx, "loadNamespace")
	defer span.End()

	defs, err := loadAllNamespaces(ctx, tx, func(original sq.SelectBuilder) sq.SelectBuilder {
		return filterer(original).Where(sq.Eq{colNamespace: namespace})
	})
	if err != nil {
		return nil, postgresRevision{}, err
	}

	if len(defs) < 1 {
		return nil, postgresRevision{}, datastore.NewNamespaceNotFoundErr(namespace)
	}

	return defs[0].Definition, defs[0].LastWrittenRevision.(postgresRevision), nil
}

func (r *pgReader) ListAllNamespaces(ctx context.Context) ([]datastore.RevisionedNamespace, error) {
	tx, txCleanup, err := r.txSource(ctx)
	if err != nil {
		return nil, err
	}
	defer txCleanup(ctx)

	nsDefsWithRevisions, err := loadAllNamespaces(ctx, tx, r.filterer)
	if err != nil {
		return nil, fmt.Errorf(errUnableToListNamespaces, err)
	}

	return nsDefsWithRevisions, err
}

func (r *pgReader) LookupNamespacesWithNames(ctx context.Context, nsNames []string) ([]datastore.RevisionedNamespace, error) {
	if len(nsNames) == 0 {
		return nil, nil
	}

	tx, txCleanup, err := r.txSource(ctx)
	if err != nil {
		return nil, err
	}
	defer txCleanup(ctx)

	clause := sq.Or{}
	for _, nsName := range nsNames {
		clause = append(clause, sq.Eq{colNamespace: nsName})
	}

	nsDefsWithRevisions, err := loadAllNamespaces(ctx, tx, func(original sq.SelectBuilder) sq.SelectBuilder {
		return r.filterer(original).Where(clause)
	})
	if err != nil {
		return nil, fmt.Errorf(errUnableToListNamespaces, err)
	}

	return nsDefsWithRevisions, err
}

func loadAllNamespaces(
	ctx context.Context,
	tx pgxcommon.DBReader,
	filterer queryFilterer,
) ([]datastore.RevisionedNamespace, error) {
	sql, args, err := filterer(readNamespace).ToSql()
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nsDefs []datastore.RevisionedNamespace
	for rows.Next() {
		var config []byte
		var version xid8

		if err := rows.Scan(&config, &version); err != nil {
			return nil, err
		}

		loaded := &core.NamespaceDefinition{}
		if err := loaded.UnmarshalVT(config); err != nil {
			return nil, fmt.Errorf(errUnableToReadConfig, err)
		}

		revision := revisionForVersion(version)

		nsDefs = append(nsDefs, datastore.RevisionedNamespace{Definition: loaded, LastWrittenRevision: revision})
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	return nsDefs, nil
}

// revisionForVersion synthesizes a snapshot where the specified version is always visible.
func revisionForVersion(version xid8) postgresRevision {
	return postgresRevision{pgSnapshot{
		xmin: version.Uint64 + 1,
		xmax: version.Uint64 + 1,
	}}
}

var _ datastore.Reader = &pgReader{}
