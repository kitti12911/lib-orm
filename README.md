# lib-orm

shared Bun ORM helpers for homelab services.

## project structure

```bash
lib-orm/
├── cmd/
│   ├── fieldmapgen/      # Bun model field-map generator
│   └── patchfieldgen/    # field-mask patch extractor generator
├── orm.go                # database setup, migrations, fixtures
├── query.go              # filter, order, and patch query helpers
├── transaction.go        # context-aware transaction provider
├── Makefile
├── go.mod
└── README.md
```

## install

```bash
go get github.com/kitti12911/lib-orm/v2
```

## packages

### orm

PostgreSQL connection setup, Bun model registration, OpenTelemetry query hooks, migrations, and fixture loading.

```go
import orm "github.com/kitti12911/lib-orm/v2"

db, err := orm.New(
    ctx,
    cfg.Database,
    orm.WithApplicationName(cfg.Service.Name),
    orm.WithModels((*models.User)(nil)),
    orm.WithTracing(cfg.Tracing.Enabled),
)
```

`orm.New` returns a wrapped database value. Use `db.Bun()` only when a caller
really needs the raw `*bun.DB`; otherwise prefer `db.IDB(ctx)` so repository
queries automatically use the active transaction when one exists.

Run service-owned migrations and fixtures:

```go
err = orm.Init(ctx, db, cfg.Database, migrations.Migrations, seeders.Fixtures, "fixtures/users.yml")
```

If migration and fixture loading need to be controlled separately:

```go
if err := orm.RunMigrations(ctx, db.Bun(), migrations.Migrations); err != nil {
    return err
}
if err := orm.LoadFixtures(ctx, db.Bun(), seeders.Fixtures, "fixtures/users.yml"); err != nil {
    return err
}
```

Wrap an existing Bun connection when another package already created it:

```go
db := orm.Wrap(existingBunDB)
defer db.Close()
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

### query helpers

Reusable Bun query helpers for validated filtering, ordering, and patch
updates.

```go
filters := orm.FiltersFromProto(req.GetFilters())
orderBy := orm.OrderByFromProto(req.GetOrderBy())

query := db.IDB(ctx).NewSelect().Model(&users)

if err := orm.ApplyFilters(query, filters, fieldmap.UserRootFields); err != nil {
    return err
}
if err := orm.ApplyOrderBy(query, orderBy, fieldmap.UserRootFields); err != nil {
    return err
}
```

`ApplyFilters` and `ApplyOrderBy` only accept fields present in the provided
field map. Column names are emitted with `bun.Ident`, and values stay
parameterized through Bun.

Supported filter operators:

- exact
- like
- case-insensitive like
- greater than / less than / greater-or-equal / less-or-equal
- null / not null
- in
- between
- exclusive between

For patch updates, build writable columns from a generated field map and block
immutable fields:

```go
columns := orm.WritableColumns(
    fieldmap.UserRootFields,
    "id",
    "created_at",
    "updated_at",
    "deleted_at",
)

query := db.IDB(ctx).NewUpdate().Model((*models.User)(nil)).Where("id = ?", id)
if err := orm.ApplyPatchFields(query, fields, columns); err != nil {
    return err
}
```

### generators

`fieldmapgen` generates Bun-aware field maps and validation helpers from model
structs:

```bash
go run github.com/kitti12911/lib-orm/v2/cmd/fieldmapgen@v2.2.0 \
    -model-dir internal/database \
    -root User \
    -out gen/database/fieldmap_generated.go \
    -package database
```

Flag guide:

- `-model-dir`: directory containing Bun model structs.
- `-root`: root model name to walk from, for example `User`.
- `-out`: generated output file.
- `-package`: package name for the generated file.

Generated output gives you maps such as `UserRootFields`,
`UserProfileFields`, and validator functions such as `IsUserRootField`. Query
helpers use these maps to validate filter, order, and patch field names before
building SQL.

`patchfieldgen` generates field-mask extraction code for PATCH handlers. It
maps request paths into table-specific field buckets and can copy nested request
values for create-if-missing flows:

```bash
go run github.com/kitti12911/lib-orm/v2/cmd/patchfieldgen@v2.2.0 \
    -file internal/feature/user/user.go \
    -root CreateParams \
    -out internal/feature/user/patch_generated.go \
    -package user \
    -fieldmap-import grpc-sandbox/gen/database \
    -root-selector params.User \
    -paths-selector params.Fields \
    -bucket root:userFields:fieldmap.IsUserRootField \
    -bucket profile:profileFields:fieldmap.IsUserProfileField \
    -bucket profile.address:addressFields:fieldmap.IsUserAddressField \
    -copy params.User.Profile:data.profile \
    -copy params.User.Profile.Address:data.address:params.User.Profile
```

Flag guide:

- `-file`: Go source file containing the patch input structs.
- `-root`: root struct type to inspect. In `grpc-sandbox`, this is
  `CreateParams` because PATCH accepts the same editable user shape.
- `-out`: generated output file.
- `-package`: package name for the generated file.
- `-fieldmap-import`: import path for generated field-map validators.
- `-root-selector`: selector for the request data inside the generated
  function. If the generated function is `patchFields(params PatchParams)` and
  values live at `params.User`, use `params.User`.
- `-paths-selector`: selector for field mask paths. In `grpc-sandbox`, this is
  `params.Fields`.
- `-bucket`: route field mask paths into table-specific output maps.
- `-copy`: copy nested pointer values from the request into patch data.

Bucket format:

```text
path_prefix:output_map:validator_func
```

Example:

```bash
-bucket profile.address:addressFields:fieldmap.IsUserAddressField
```

This means paths like `profile.address.city` go into `data.addressFields`, and
the final key `city` must pass `fieldmap.IsUserAddressField("city")`.

Use `root` as the prefix for top-level fields:

```bash
-bucket root:userFields:fieldmap.IsUserRootField
```

Copy format:

```text
source_pointer:target_value[:guard_pointer,guard_pointer]
```

Example:

```bash
-copy params.User.Profile.Address:data.address:params.User.Profile
```

This generates a guarded copy:

```go
if params.User.Profile != nil && params.User.Profile.Address != nil {
    data.address = *params.User.Profile.Address
}
```

Use `-copy` when service code may need the full nested value, usually to create
a missing child row before applying PATCH field updates. Buckets are for SQL
field maps; copies are for carrying nested create data.

## requirements

- go 1.26 or higher

Optional:

- [prettier](https://prettier.io/) for Markdown, YAML, JSON, and JSONC formatting

## available commands

```bash
make tidy
make fmt
make pretty
make format
make test
make cov
```
