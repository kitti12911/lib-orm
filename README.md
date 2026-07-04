# lib-orm

shared Bun ORM helpers for homelab services.

## project structure

```bash
lib-orm/
├── cmd/
│   └── mapgen/           # code generator: fields, patch, and proto subcommands
├── orm.go                # database setup, migrations, fixtures
├── query.go              # filter, order, and patch query helpers
├── transaction.go        # context-aware transaction provider
├── uuid.go               # dialect-portable UUID scanner/value
├── Makefile
├── go.mod
└── README.md
```

## install

```bash
go get github.com/kitti12911/lib-orm/v3
```

## ci commands

reusable CI entrypoints live in `scripts/ci/` so GitHub Actions can call the
same commands with workflow-specific orchestration around them.

| command                                    | purpose                          |
| ------------------------------------------ | -------------------------------- |
| `./scripts/ci/go-lint.sh`                  | run `go vet` and `golangci-lint` |
| `./scripts/ci/go-test.sh`                  | run tests with coverage          |
| `./scripts/ci/markdownlint.sh`             | run Markdown linting             |
| `./scripts/ci/security-scan.sh`            | run `govulncheck` and Semgrep    |
| `./scripts/ci/supply-chain-scan.sh`        | run Trivy and Gitleaks           |
| `./scripts/ci/semantic-release-publish.sh` | publish the semantic release     |

GitHub Actions uses `TOOLCHAIN_REGISTRY` and `TOOLCHAIN_IMAGE_NAMESPACE` to
resolve the shared Zot toolchain images.

## packages

### orm

PostgreSQL or SQL Server connection setup, Bun model registration, OpenTelemetry query hooks, migrations, and fixture loading.

```go
import orm "github.com/kitti12911/lib-orm/v3"

db, err := orm.New(
    ctx,
    cfg.Database,
    orm.WithApplicationName(cfg.Service.Name),
    orm.WithModels((*models.User)(nil)),
    orm.WithTracing(cfg.Tracing.Enabled),
)
```

`Config.Driver` selects the dialect. Leave it empty or set
`orm.DriverPostgres` for PostgreSQL (default, backward compatible) and set
`orm.DriverMSSQL` for SQL Server. `Insecure` disables TLS for `pgdriver` and
maps to `encrypt=disable` for the SQL Server driver. Fixture loading emits
`ON CONFLICT DO NOTHING` only for PostgreSQL; SQL Server fixtures fall back to
plain inserts.

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

### uuid

`orm.UUID` is a dialect-portable UUID column rendered as its canonical
lowercase string. It scans SQL Server `uniqueidentifier` bytes and PostgreSQL
`uuid`/text alike, so models can use a plain `Model()` select instead of
per-dialect `CONVERT` expressions. Its zero value is the empty string, which
`Value` encodes as `NULL` so a column `DEFAULT` can generate the id on insert.

```go
type User struct {
    bun.BaseModel `bun:"table:users,alias:u"`

    ID   orm.UUID `bun:"id,pk,nullzero,default:gen_random_uuid()"`
    Name string   `bun:"name"`
}

var id orm.UUID
_, err := db.IDB(ctx).NewInsert().
    Model(user).
    Returning("id").
    Exec(ctx, &id)

err = db.IDB(ctx).NewSelect().
    Model(user).
    Where("u.id = ?", id.String()).
    Scan(ctx)
```

Nullable foreign keys can use `*orm.UUID`.

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

`mapgen fields` generates Bun-aware field maps and validation helpers from
model structs:

```bash
go run github.com/kitti12911/lib-orm/v3/cmd/mapgen@v3.5.0 fields \
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

`mapgen patch` generates field-mask extraction code for PATCH handlers from a
YAML config. It maps request paths into table-specific field buckets and can
copy nested request values for create-if-missing flows:

```bash
go run github.com/kitti12911/lib-orm/v3/cmd/mapgen@v3.5.0 patch \
    -config internal/feature/user/patchfields.yaml
```

Config guide:

- `file`: Go source file containing the patch input structs.
- `root`: root struct type to inspect. In `grpc-sandbox`, this is
  `CreateParams` because PATCH accepts the same editable user shape.
- `out`: generated output file.
- `package`: package name for the generated file.
- `fieldmap_import`: import path for generated field-map validators.
- `root_selector`: selector for the request data inside the generated
  function. If the generated function is `patchFields(params PatchParams)` and
  values live at `params.User`, use `params.User`.
- `paths_selector`: selector for field mask paths. In `grpc-sandbox`, this is
  `params.Fields`.
- `buckets`: route field mask paths into table-specific output maps.
- `copies`: copy nested pointer values from the request into patch data.

Bucket format:

```yaml
buckets:
    - path: profile.address
      map_field: addressFields
```

This means paths like `profile.address.city` go into `data.addressFields`, and
the generated dispatcher trims the `profile.address.` prefix before storing the
final key `city`.

Omit `path` for top-level fields:

```yaml
buckets:
    - map_field: userFields
```

Copy format:

```yaml
copies:
    - source: params.User.Profile.Address
      target: data.address
      guards:
          - params.User.Profile
```

This generates a guarded copy:

```go
if params.User.Profile != nil && params.User.Profile.Address != nil {
    data.address = *params.User.Profile.Address
}
```

Use `copies` when service code may need the full nested value, usually to create
a missing child row before applying PATCH field updates. Buckets are for SQL
field maps; copies are for carrying nested create data.

`mapgen proto` generates one-to-one mapping functions between Go structs and
protobuf messages. It handles Bun models and plain service-layer structs, can
emit only `to_proto`, only `from_proto`, or both directions, and can merge
multiple targets into one output file:

```bash
go run github.com/kitti12911/lib-orm/v3/cmd/mapgen@v3.5.0 proto \
    -config protomapgen.yaml
```

Use `converters:` for enum, relation, or custom-type bridges, `exclude:` for
fields without a proto counterpart, `target_pointer: false` for value-returning
parameter structs, and `unwrap:` when a request message wraps the payload in a
nested field.

## prerequisites

Install the third-party CLIs this repo expects. Match `go.mod` for the Go
version.

### macOS (Homebrew)

```bash
brew install go golangci-lint prettier markdownlint-cli2
```

## available commands

| Command       | Description                                     |
| ------------- | ----------------------------------------------- |
| `make tidy`   | Run `go mod tidy`                               |
| `make lint`   | Run Go and Markdown linting                     |
| `make fmt`    | Format Go code with `go fmt`                    |
| `make pretty` | Format Markdown, YAML, JSON, and JSONC          |
| `make format` | Run Go and document/config formatting           |
| `make test`   | Run tests with the race detector                |
| `make cov`    | Generate and open an HTML coverage report       |
| `make fix`    | Apply standard Go source rewrites with `go fix` |
