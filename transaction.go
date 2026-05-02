package orm

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
)

type txKey struct{ db *bun.DB }

type TransactionProvider struct {
	db *bun.DB
}

func NewTransactionProvider(db *bun.DB) *TransactionProvider {
	return &TransactionProvider{db: db}
}

func (p *TransactionProvider) Transaction(ctx context.Context, fn func(context.Context) error) error {
	return p.TransactionWithOptions(ctx, nil, fn)
}

func (p *TransactionProvider) TransactionWithOptions(
	ctx context.Context,
	opts *sql.TxOptions,
	fn func(context.Context) error,
) error {
	if _, ok := p.TxFromContext(ctx); ok {
		return fn(ctx)
	}

	tx, err := p.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	txCtx := context.WithValue(ctx, txKey{db: p.db}, tx)
	if err := fn(txCtx); err != nil {
		return err
	}

	return tx.Commit()
}

func (p *TransactionProvider) TxFromContext(ctx context.Context) (bun.Tx, bool) {
	tx, ok := ctx.Value(txKey{db: p.db}).(bun.Tx)
	return tx, ok
}

func (p *TransactionProvider) IDB(ctx context.Context) bun.IDB {
	if tx, ok := p.TxFromContext(ctx); ok {
		return tx
	}
	return p.db
}
