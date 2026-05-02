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
