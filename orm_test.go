package orm

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mssqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/migrate"
	"github.com/uptrace/bun/schema"
)

type testModel struct {
	bun.BaseModel `bun:"table:test_models,alias:t"`
	ID            int    `bun:"id,pk"`
	Name          string `bun:"name"`
}

func TestOptions(t *testing.T) {
	opts := options{}
	WithApplicationName("svc")(&opts)
	WithModels((*testModel)(nil))(&opts)
	WithTracing(true)(&opts)

	assert.Equal(t, "svc", opts.appName)
	assert.Len(t, opts.models, 1)
	assert.True(t, opts.tracing)
}

func TestNewPingError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	db, err := New(ctx, Config{
		Host:     "localhost",
		Port:     "5432",
		User:     "user",
		Password: "pass",
		Database: "app",
		Insecure: true,
		Pool: PoolConfig{
			MaxConns:        2,
			MinConns:        1,
			MaxConnLifetime: time.Minute,
			MaxConnIdleTime: time.Second,
		},
	}, WithApplicationName("svc"), WithModels((*testModel)(nil)), WithTracing(true))
	require.Error(t, err)
	assert.Nil(t, db)
	assert.ErrorContains(t, err, "orm: ping")
}

func TestNewMSSQLPingError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	db, err := New(ctx, Config{
		Driver:   DriverMSSQL,
		Host:     "localhost",
		Port:     "1433",
		User:     "sa",
		Password: "pass",
		Database: "app",
		Insecure: true,
	}, WithApplicationName("svc"))
	require.Error(t, err)
	assert.Nil(t, db)
	assert.ErrorContains(t, err, "orm: ping")
}

func TestNewUnsupportedDriver(t *testing.T) {
	db, err := New(context.Background(), Config{Driver: "oracle"})
	require.Error(t, err)
	assert.Nil(t, db)
	assert.ErrorContains(t, err, "unsupported driver")
}

func TestMSSQLDSN(t *testing.T) {
	dsn := mssqlDSN(Config{
		Host:     "db.example",
		Port:     "1433",
		User:     "sa",
		Password: "p@ss word",
		Database: "app",
		Insecure: true,
	}, "svc")

	assert.Contains(t, dsn, "sqlserver://")
	assert.Contains(t, dsn, "db.example:1433")
	assert.Contains(t, dsn, "database=app")
	assert.Contains(t, dsn, "app+name=svc")
	assert.Contains(t, dsn, "encrypt=disable")
}

func TestNewDB(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	mock.ExpectPing()

	db, err := newDB(context.Background(), sqlDB, pgdialect.New(), Config{
		Database: "app",
		Pool: PoolConfig{
			MaxConns:        2,
			MinConns:        1,
			MaxConnLifetime: time.Minute,
			MaxConnIdleTime: time.Second,
		},
	}, options{
		models:  []any{(*testModel)(nil)},
		tracing: true,
	})
	require.NoError(t, err)
	require.NotNil(t, db)
	defer db.Close()

	assert.Equal(t, 2, sqlDB.Stats().MaxOpenConnections)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNewWrappedDB(t *testing.T) {
	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	mock.ExpectPing()

	db, err := newWrappedDB(context.Background(), sqlDB, pgdialect.New(), Config{Database: "app"}, options{})
	require.NoError(t, err)
	require.NotNil(t, db)
	defer db.Close()

	assert.NotNil(t, db.Bun())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInitNoop(t *testing.T) {
	assert.NoError(t, Init(context.Background(), nil, Config{}, nil, nil))
}

func TestInitNoopWithDB(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	err := Init(context.Background(), Wrap(db), Config{}, nil, nil)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInitReturnsMigrationError(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS bun_migrations`).
		WillReturnError(errors.New("create failed"))

	err := Init(context.Background(), Wrap(db), Config{RunMigrations: true}, migrate.NewMigrations(), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "orm: init migrations")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInitReturnsSeederError(t *testing.T) {
	db, _ := newMockBunDB(t)
	defer db.Close()

	err := Init(context.Background(), Wrap(db), Config{RunSeeders: true}, nil, fstest.MapFS{}, "missing.yml")
	require.Error(t, err)
	assert.ErrorContains(t, err, "orm: load fixtures")
}

func TestRunMigrationsInitError(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS bun_migrations`).
		WillReturnError(errors.New("create failed"))

	err := RunMigrations(context.Background(), db, migrate.NewMigrations())
	require.Error(t, err)
	assert.ErrorContains(t, err, "orm: init migrations")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunMigrationsMigrateError(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS bun_migrations`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS bun_migration_locks`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := RunMigrations(context.Background(), db, migrate.NewMigrations())
	require.Error(t, err)
	assert.ErrorContains(t, err, "orm: migrate")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunMigrations(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS bun_migrations`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS bun_migration_locks`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT \* FROM bun_migrations`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "group_id", "migrated_at"}).
			AddRow(1, "20260101000000", 1, time.Now()))

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260101000000"})

	err := RunMigrations(context.Background(), db, migrations)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadFixtures(t *testing.T) {
	db, mock := newMockBunDB(t)
	defer db.Close()

	mock.ExpectExec(`INSERT INTO "test_models" AS "t" \("id", "name"\) VALUES \(1, 'alice'\) ON CONFLICT DO NOTHING`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := LoadFixtures(context.Background(), db, fstest.MapFS{
		"fixtures.yml": {
			Data: []byte(`
- model: TestModel
  rows:
    - id: 1
      name: alice
`),
		},
	}, "fixtures.yml")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadFixturesMSSQL(t *testing.T) {
	db, mock := newMockBunDBWithDialect(t, mssqldialect.New())
	defer db.Close()

	mock.ExpectExec(`INSERT INTO "test_models" \("id", "name"\) VALUES \(1, N'alice'\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := LoadFixtures(context.Background(), db, fstest.MapFS{
		"fixtures.yml": {
			Data: []byte(`
- model: TestModel
  rows:
    - id: 1
      name: alice
`),
		},
	}, "fixtures.yml")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadFixturesError(t *testing.T) {
	db, _ := newMockBunDB(t)
	defer db.Close()

	err := LoadFixtures(context.Background(), db, fstest.MapFS{}, "missing.yml")
	require.Error(t, err)
	assert.ErrorContains(t, err, "orm: load fixtures")
}

func TestApplyPoolConfig(t *testing.T) {
	sqlDB, _, err := sqlmock.New()
	require.NoError(t, err)
	defer sqlDB.Close()

	applyPoolConfig(sqlDB, PoolConfig{
		MaxConns:        2,
		MinConns:        1,
		MaxConnLifetime: time.Minute,
		MaxConnIdleTime: time.Second,
	})

	stats := sqlDB.Stats()
	assert.Equal(t, 2, stats.MaxOpenConnections)
}

func newMockBunDB(t *testing.T) (*bun.DB, sqlmock.Sqlmock) {
	t.Helper()
	return newMockBunDBWithDialect(t, pgdialect.New())
}

func newMockBunDBWithDialect(t *testing.T, d schema.Dialect) (*bun.DB, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	db := bun.NewDB(sqlDB, d)
	db.RegisterModel((*testModel)(nil))
	return db, mock
}

var _ = sql.ErrNoRows
