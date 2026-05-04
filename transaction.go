package orm

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
)

type txKey struct{ db *bun.DB }

type TransactionProvider struct {
	db *bun.DB
}

type DB struct {
	db         *bun.DB
	txProvider *TransactionProvider
}

func Wrap(db *bun.DB) *DB {
	return &DB{
		db:         db,
		txProvider: NewTransactionProvider(db),
	}
}

func NewTransactionProvider(db *bun.DB) *TransactionProvider {
	return &TransactionProvider{db: db}
}

func (db *DB) Bun() *bun.DB {
	return db.db
}

func (db *DB) Close() error {
	if err := db.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}

func (db *DB) Transaction(ctx context.Context, fn func(context.Context) error) error {
	return db.txProvider.Transaction(ctx, fn)
}

func (db *DB) TransactionWithOptions(
	ctx context.Context,
	opts *sql.TxOptions,
	fn func(context.Context) error,
) error {
	return db.txProvider.TransactionWithOptions(ctx, opts, fn)
}

func (db *DB) TxFromContext(ctx context.Context) (bun.Tx, bool) {
	return db.txProvider.TxFromContext(ctx)
}

func (db *DB) IDB(ctx context.Context) bun.IDB {
	return db.txProvider.IDB(ctx)
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
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	txCtx := context.WithValue(ctx, txKey{db: p.db}, tx)
	if err := fn(txCtx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
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
