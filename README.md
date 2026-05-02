# lib-orm

shared Bun ORM helpers for homelab services.

## install

```bash
go get github.com/kitti12911/lib-orm
```

## packages

### orm

PostgreSQL connection setup, Bun model registration, OpenTelemetry query hooks, migrations, and fixture loading.

```go
import orm "github.com/kitti12911/lib-orm"

db, err := orm.New(
    ctx,
    cfg.Database,
    orm.WithApplicationName(cfg.Service.Name),
    orm.WithModels((*models.User)(nil)),
    orm.WithTracing(cfg.Tracing.Enabled),
)
```

Run service-owned migrations and fixtures:

```go
err = orm.Init(ctx, db, cfg.Database, migrations.Migrations, seeders.Fixtures, "fixtures/users.yml")
```

Create one transaction provider at composition time:

```go
txProvider := orm.NewTransactionProvider(db)
```

Service code should depend on a narrow transaction interface, not the database
connection:

```go
type transactor interface {
    Transaction(context.Context, func(context.Context) error) error
}
```

Run local transactional work from the service. Nested calls reuse the active
transaction:

```go
err = txProvider.Transaction(ctx, func(ctx context.Context) error {
    if err := userRepo.UpdateUser(ctx, params); err != nil {
        return err
    }
    return profileRepo.UpdateProfile(ctx, params)
})
```

Repositories can use the provider to route queries to either the active
transaction or the base database:

```go
_, err := txProvider.IDB(ctx).NewUpdate().
    Model((*models.User)(nil)).
    Set("updated_at = now()").
    Where("id = ?", id).
    Exec(ctx)
```
