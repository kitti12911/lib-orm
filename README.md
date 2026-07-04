# lib-orm

shared Bun ORM helpers for homelab services.

## project structure

```bash
lib-orm/
├── cmd/
│   └── mapgen/           # zero-config code generator: map, fields, patch, filter
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
go get github.com/kitti12911/lib-orm/v4
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
import orm "github.com/kitti12911/lib-orm/v4"

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

`Filter` is recursive: a leaf carries `Col/Op/Val(s)`; a group carries `Logic`
(`AND`, default, or `OR`) plus nested `Filters`. One type expresses everything
from a single predicate to arbitrarily nested boolean trees.

```go
filter := orm.FilterFromProto(req.GetFilter())
orderBy := orm.OrderByFromProto(req.GetOrderBy())

query := db.IDB(ctx).NewSelect().Model(&users)

if err := orm.ApplyFilter(query, filter, fieldmap.UserColumns, nil); err != nil {
    return err
}
if err := orm.ApplyOrderBy(query, orderBy, fieldmap.UserColumns); err != nil {
    return err
}
```

`ApplyFilter` and `ApplyOrderBy` only accept fields present in the provided
field map. Column names are emitted with `bun.Ident`, and values stay
parameterized through Bun. Groups compose through `WhereGroup`, so
`A AND (B OR C)` nests correctly at any depth.

The last argument backs **virtual/composite columns** with custom SQL: a
`map[string]orm.FilterExpr` consulted before the column map. A `FilterExpr`
returns an expression fragment plus args, so custom leaves compose inside
AND/OR groups like any physical column. `mapgen filter` builds this registry
from `//mapgen:filter` directives (see generators).

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

`mapgen` is **zero-config**: every subcommand discovers its inputs by naming
convention from the repo root and takes no flags beyond an optional `-C <dir>`.
There are no config files; escape hatches are source-comment directives.

```bash
go run github.com/kitti12911/lib-orm/v4/cmd/mapgen@v4.0.0 fields
go run github.com/kitti12911/lib-orm/v4/cmd/mapgen@v4.0.0 map
go run github.com/kitti12911/lib-orm/v4/cmd/mapgen@v4.0.0 patch
go run github.com/kitti12911/lib-orm/v4/cmd/mapgen@v4.0.0 filter
```

Conventions the generators read:

| Input | Convention |
| --- | --- |
| Bun models | `internal/database` — structs with a `bun:"table:..."` tag; roots are models no other model has-one/has-many/many-to-many targets |
| Feature packages | `internal/feature/<f>` — root entity is PascalCase of `<f>` (`user` → `User`) |
| Params structs | `Create<X>Params` maps from proto `<Root><X>` (`CreateParams` → `User`); `UpdateParams` = create fields + `ID`; `PatchParams` = one payload field + `Fields []string` |
| Proto types | `gen/grpc/<f>/v1/*.pb.go`, parsed directly — field sets intersect, so fields missing on either side are skipped automatically |
| huma gateways | `internal/api/<domain>/v1` (detected via `internal/api`) — models pair with the proto package imported by the domain; `<Rpc>Output{Body}` types become envelope mappers |

What each subcommand emits:

- **fields** → `gen/database/fieldmap_generated.go`: `<Model>Fields` maps and
  `<Root>Columns` (dotted path → qualified column) per root model.
- **map** (gRPC layout) → `internal/feature/<f>/mapper_generated.go`:
  `toProto<Model>` for the root model and its relations,
  `<params>FromProto` for every `Create<X>Params`, plus string↔enum bridges
  generated from the proto enum values (`USER_STATUS_ACTIVE` ↔ `"active"`).
- **map** (huma layout) → `internal/api/<domain>/v1/mapper_generated.go`:
  model mappers in both directions, envelope wrappers
  (`<rpc>OutputFromProto`), list+pagination flattening, and enum bridges.
- **patch** → `internal/feature/<f>/patch_generated.go`: the `patchData`
  struct and `patchFields(params PatchParams) patchData` dispatcher, with
  buckets and guarded copies derived from the payload struct's
  `field:"..."`-tagged shape.
- **filter** → `internal/feature/<f>/filter_generated.go`: `applyFilter` /
  `applyOrderBy` wrappers over the generated column map plus the
  custom-filter registry.

Directives (comments in your source, never config files):

- `//mapgen:ignore` — package doc comment; skips the feature/domain entirely
  (use for hand-written mappers, e.g. fallible converters).
- `//mapgen:proto=<Message>` — on a params struct; overrides the derived proto
  message when names are irregular.
- `//mapgen:filter col=<name>` — on a `func(f orm.Filter) (string, []any, error)`;
  registers custom SQL for a virtual column so auto-mapping and composite
  filtering coexist:

```go
//mapgen:filter col=full_name
func filterFullName(f orm.Filter) (string, []any, error) {
    return "concat_ws(' ', p.first_name, p.last_name) ILIKE ?",
        []any{"%" + fmt.Sprint(f.Val) + "%"}, nil
}
```

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
