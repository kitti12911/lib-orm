package orm

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bunotel"
	"github.com/uptrace/bun/migrate"
)

type Option func(*options)

type options struct {
	appName string
	models  []any
	tracing bool
}

func WithApplicationName(name string) Option {
	return func(opts *options) {
		opts.appName = name
	}
}

func WithModels(models ...any) Option {
	return func(opts *options) {
		opts.models = append(opts.models, models...)
	}
}

func WithTracing(enabled bool) Option {
	return func(opts *options) {
		opts.tracing = enabled
	}
}

func New(ctx context.Context, cfg Config, opts ...Option) (*bun.DB, error) {
	options := options{}
	for _, opt := range opts {
		opt(&options)
	}

	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithAddr(net.JoinHostPort(cfg.Host, cfg.Port)),
		pgdriver.WithUser(cfg.User),
		pgdriver.WithPassword(cfg.Password),
		pgdriver.WithDatabase(cfg.Database),
		pgdriver.WithApplicationName(options.appName),
		pgdriver.WithInsecure(cfg.Insecure),
	))

	return newDB(ctx, sqldb, cfg, options)
}

func newDB(ctx context.Context, sqldb *sql.DB, cfg Config, options options) (*bun.DB, error) {
	applyPoolConfig(sqldb, cfg.Pool)

	db := bun.NewDB(sqldb, pgdialect.New())
	if len(options.models) > 0 {
		db.RegisterModel(options.models...)
	}

	if options.tracing {
		db.AddQueryHook(bunotel.NewQueryHook(bunotel.WithDBName(cfg.Database)))
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("orm: ping: %w", err)
	}

	return db, nil
}

func Init(
	ctx context.Context,
	db *bun.DB,
	cfg Config,
	migrations *migrate.Migrations,
	fixtures fs.FS,
	fixtureNames ...string,
) error {
	if cfg.RunMigrations && migrations != nil {
		if err := RunMigrations(ctx, db, migrations); err != nil {
			return err
		}
	}

	if cfg.RunSeeders && fixtures != nil && len(fixtureNames) > 0 {
		if err := LoadFixtures(ctx, db, fixtures, fixtureNames...); err != nil {
			return err
		}
	}

	return nil
}

func RunMigrations(ctx context.Context, db *bun.DB, migrations *migrate.Migrations) error {
	migrator := migrate.NewMigrator(
		db,
		migrations,
		migrate.WithMarkAppliedOnSuccess(true),
	)

	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("orm: init migrations: %w", err)
	}

	if _, err := migrator.Migrate(ctx); err != nil {
		return fmt.Errorf("orm: migrate: %w", err)
	}

	return nil
}

func LoadFixtures(ctx context.Context, db *bun.DB, fixtures fs.FS, names ...string) error {
	loader := dbfixture.New(db, dbfixture.WithBeforeInsert(func(ctx context.Context, data *dbfixture.BeforeInsertData) error {
		data.Query.On("CONFLICT DO NOTHING")
		return nil
	}))

	if err := loader.Load(ctx, fixtures, names...); err != nil {
		return fmt.Errorf("orm: load fixtures: %w", err)
	}

	return nil
}

func applyPoolConfig(sqldb *sql.DB, cfg PoolConfig) {
	if cfg.MaxConns > 0 {
		sqldb.SetMaxOpenConns(int(cfg.MaxConns))
	}

	if cfg.MinConns > 0 {
		sqldb.SetMaxIdleConns(int(cfg.MinConns))
	}

	if cfg.MaxConnLifetime > 0 {
		sqldb.SetConnMaxLifetime(cfg.MaxConnLifetime)
	}

	if cfg.MaxConnIdleTime > 0 {
		sqldb.SetConnMaxIdleTime(cfg.MaxConnIdleTime)
	}
}
