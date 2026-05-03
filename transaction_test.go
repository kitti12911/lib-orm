package orm

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransactionProvider(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	provider := NewTransactionProvider(db)
	assert.Same(t, db, provider.IDB(context.Background()))

	mock.ExpectBegin()
	mock.ExpectCommit()
	err := provider.Transaction(context.Background(), func(ctx context.Context) error {
		tx, ok := provider.TxFromContext(ctx)
		require.True(t, ok)
		assert.Equal(t, tx, provider.IDB(ctx))
		return nil
	})
	require.NoError(t, err)

	callbackErr := errors.New("callback failed")
	mock.ExpectBegin()
	mock.ExpectRollback()
	err = provider.TransactionWithOptions(context.Background(), &sql.TxOptions{ReadOnly: true}, func(ctx context.Context) error {
		return callbackErr
	})
	require.ErrorIs(t, err, callbackErr)

	mock.ExpectBegin()
	mock.ExpectCommit()
	err = provider.Transaction(context.Background(), func(ctx context.Context) error {
		return provider.Transaction(ctx, func(ctx context.Context) error {
			return nil
		})
	})
	require.NoError(t, err)
}

func TestTransactionProviderBeginError(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	beginErr := errors.New("begin failed")
	mock.ExpectBegin().WillReturnError(beginErr)

	provider := NewTransactionProvider(db)
	err := provider.Transaction(context.Background(), func(ctx context.Context) error {
		return nil
	})
	require.ErrorIs(t, err, beginErr)
}
