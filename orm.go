package orm

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net"
	"net/url"

	// register "sqlserver" sql driver used by the MSSQL dialect.
	_ "github.com/microsoft/go-mssqldb"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/mssqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/extra/bunotel"
	"github.com/uptrace/bun/migrate"
	"github.com/uptrace/bun/schema"
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

func New(ctx context.Context, cfg Config, opts ...Option) (*DB, error) {
	cfgOptions := options{}
	for _, opt := range opts {
		opt(&cfgOptions)
	}

	sqldb, d, err := openSQLDB(cfg, cfgOptions)
	if err != nil {
		return nil, err
	}

	return newWrappedDB(ctx, sqldb, d, cfg, cfgOptions)
}

func openSQLDB(cfg Config, opts options) (*sql.DB, schema.Dialect, error) {
	switch cfg.Driver {
	case "", DriverPostgres:
		sqldb := sql.OpenDB(pgdriver.NewConnector(
			pgdriver.WithAddr(net.JoinHostPort(cfg.Host, cfg.Port)),
			pgdriver.WithUser(cfg.User),
			pgdriver.WithPassword(cfg.Password),
			pgdriver.WithDatabase(cfg.Database),
			pgdriver.WithApplicationName(opts.appName),
			pgdriver.WithInsecure(cfg.Insecure),
		))
		return sqldb, pgdialect.New(), nil
	case DriverMSSQL:
		sqldb, err := sql.Open("sqlserver", mssqlDSN(cfg, opts.appName))
		if err != nil {
			return nil, nil, fmt.Errorf("orm: open sqlserver: %w", err)
		}
		return sqldb, mssqldialect.New(), nil
	default:
		return nil, nil, fmt.Errorf("orm: unsupported driver %q", cfg.Driver)
	}
}

func mssqlDSN(cfg Config, appName string) string {
	q := url.Values{}
	q.Set("database", cfg.Database)
	if appName != "" {
		q.Set("app name", appName)
	}
	if cfg.Insecure {
		q.Set("encrypt", "disable")
	}

	u := url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, cfg.Port),
		RawQuery: q.Encode(),
	}
	return u.String()
}

func newWrappedDB(ctx context.Context, sqldb *sql.DB, d schema.Dialect, cfg Config, options options) (*DB, error) {
	db, err := newDB(ctx, sqldb, d, cfg, options)
	if err != nil {
		return nil, err
	}

	return Wrap(db), nil
}

func newDB(ctx context.Context, sqldb *sql.DB, d schema.Dialect, cfg Config, options options) (*bun.DB, error) {
	applyPoolConfig(sqldb, cfg.Pool)

	db := bun.NewDB(sqldb, d)
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
	db *DB,
	cfg Config,
	migrations *migrate.Migrations,
	fixtures fs.FS,
	fixtureNames ...string,
) error {
	if db == nil {
		return nil
	}

	if cfg.RunMigrations && migrations != nil {
		if err := RunMigrations(ctx, db.Bun(), migrations); err != nil {
			return err
		}
	}

	if cfg.RunSeeders && fixtures != nil && len(fixtureNames) > 0 {
		if err := LoadFixtures(ctx, db.Bun(), fixtures, fixtureNames...); err != nil {
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
	loaderOpts := []dbfixture.FixtureOption{}
	if db.Dialect().Name() == dialect.PG {
		loaderOpts = append(loaderOpts, dbfixture.WithBeforeInsert(func(ctx context.Context, data *dbfixture.BeforeInsertData) error {
			data.Query.On("CONFLICT DO NOTHING")
			return nil
		}))
	}

	loader := dbfixture.New(db, loaderOpts...)

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
