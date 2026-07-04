package orm

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

type testProtoFilterOp int32

const (
	testProtoFilterOpUnspecified      testProtoFilterOp = 0
	testProtoFilterOpExact            testProtoFilterOp = 1
	testProtoFilterOpLike             testProtoFilterOp = 2
	testProtoFilterOpGT               testProtoFilterOp = 3
	testProtoFilterOpLT               testProtoFilterOp = 4
	testProtoFilterOpGTE              testProtoFilterOp = 5
	testProtoFilterOpLTE              testProtoFilterOp = 6
	testProtoFilterOpNull             testProtoFilterOp = 7
	testProtoFilterOpNotNull          testProtoFilterOp = 8
	testProtoFilterOpLikeCI           testProtoFilterOp = 9
	testProtoFilterOpIn               testProtoFilterOp = 10
	testProtoFilterOpBetween          testProtoFilterOp = 11
	testProtoFilterOpBetweenExclusive testProtoFilterOp = 12
)

type testProtoLogicalOp int32

const (
	testProtoLogicalOpUnspecified testProtoLogicalOp = 0
	testProtoLogicalOpAnd         testProtoLogicalOp = 1
	testProtoLogicalOpOr          testProtoLogicalOp = 2
)

type testProtoOrderDirection int32

const (
	testProtoOrderDirectionUnspecified testProtoOrderDirection = 0
	testProtoOrderDirectionAsc         testProtoOrderDirection = 1
	testProtoOrderDirectionDesc        testProtoOrderDirection = 2
)

type testProtoFilter struct {
	col     string
	op      testProtoFilterOp
	val     string
	vals    []string
	logic   testProtoLogicalOp
	filters []testProtoFilter
}

func (f testProtoFilter) GetCol() string                { return f.col }
func (f testProtoFilter) GetOp() testProtoFilterOp      { return f.op }
func (f testProtoFilter) GetVal() string                { return f.val }
func (f testProtoFilter) GetVals() []string             { return f.vals }
func (f testProtoFilter) GetLogic() testProtoLogicalOp  { return f.logic }
func (f testProtoFilter) GetFilters() []testProtoFilter { return f.filters }

type testProtoOrderBy struct {
	col   string
	order testProtoOrderDirection
}

func (o testProtoOrderBy) GetCol() string                    { return o.col }
func (o testProtoOrderBy) GetOrder() testProtoOrderDirection { return o.order }

type testPatchUser struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	ID    string `bun:"id"`
	Email string `bun:"email"`
}

func renderSelect(t *testing.T, query *bun.SelectQuery) string {
	t.Helper()
	sql, err := query.AppendQuery(query.DB().QueryGen(), nil)
	require.NoError(t, err)
	return string(sql)
}

func newSelect() *bun.SelectQuery {
	return bun.NewDB(nil, pgdialect.New()).NewSelect()
}

var testColumns = map[string]string{
	"email":      "u.email",
	"age":        "u.age",
	"status":     "u.status",
	"created_at": "u.created_at",
	"deleted_at": "u.deleted_at",
}

// group builds an AND group of the given leaves.
func andGroup(children ...Filter) Filter { return Filter{Logic: LogicalOpAnd, Filters: children} }
func orGroup(children ...Filter) Filter  { return Filter{Logic: LogicalOpOr, Filters: children} }

func TestApplyFilterAllLeafOps(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, andGroup(
		Filter{Col: "email", Op: FilterOpLike, Val: "kit"},
		Filter{Col: "email", Op: FilterOpLikeCI, Val: "Kit"},
		Filter{Col: "age", Op: FilterOpGTE, Val: "18"},
		Filter{Col: "status", Op: FilterOpIn, Vals: []any{"active", "pending"}},
		Filter{Col: "created_at", Op: FilterOpBetween, Vals: []any{"2026-01-01", "2026-01-31"}},
		Filter{Col: "age", Op: FilterOpBetweenExclusive, Vals: []any{"18", "60"}},
		Filter{Col: "email", Op: FilterOpNotNull},
	), testColumns, nil)

	require.NoError(t, err)
	sql := renderSelect(t, query)
	assert.Contains(t, sql, `"u"."email" LIKE '%kit%'`)
	assert.Contains(t, sql, `LOWER("u"."email") LIKE LOWER('%Kit%')`)
	assert.Contains(t, sql, `"u"."age" >= '18'`)
	assert.Contains(t, sql, `"u"."status" IN ('active', 'pending')`)
	assert.Contains(t, sql, `"u"."created_at" BETWEEN '2026-01-01' AND '2026-01-31'`)
	assert.Contains(t, sql, `"u"."age" > '18' AND "u"."age" < '60'`)
	assert.Contains(t, sql, `"u"."email" IS NOT NULL`)
}

func TestApplyFilterSingleLeafRoot(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, Filter{Col: "email", Op: FilterOpExact, Val: "kit@example.com"}, testColumns, nil)

	require.NoError(t, err)
	assert.Contains(t, renderSelect(t, query), `"u"."email" = 'kit@example.com'`)
}

func TestApplyFilterEmptyIsNoop(t *testing.T) {
	query := newSelect()

	require.NoError(t, ApplyFilter(query, Filter{}, testColumns, nil))
	require.NoError(t, ApplyFilter(query, andGroup(), testColumns, nil))

	assert.NotContains(t, renderSelect(t, query), "WHERE")
}

func TestApplyFilterOrGroup(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, orGroup(
		Filter{Col: "status", Op: FilterOpExact, Val: "active"},
		Filter{Col: "status", Op: FilterOpExact, Val: "pending"},
	), testColumns, nil)

	require.NoError(t, err)
	sql := renderSelect(t, query)
	assert.Contains(t, sql, `"u"."status" = 'active'`)
	assert.Contains(t, sql, `"u"."status" = 'pending'`)
	assert.Contains(t, sql, " OR ")
}

func TestApplyFilterNestedGroup(t *testing.T) {
	query := newSelect()

	// age >= 18 AND (status = active OR status = pending)
	err := ApplyFilter(query, andGroup(
		Filter{Col: "age", Op: FilterOpGTE, Val: "18"},
		orGroup(
			Filter{Col: "status", Op: FilterOpExact, Val: "active"},
			Filter{Col: "status", Op: FilterOpExact, Val: "pending"},
		),
	), testColumns, nil)

	require.NoError(t, err)
	sql := renderSelect(t, query)
	assert.Contains(t, sql, `"u"."age" >= '18'`)
	assert.Contains(t, sql, `"u"."status" = 'active'`)
	assert.Contains(t, sql, " OR ")
}

func TestApplyFilterGroupInGroup(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, andGroup(
		andGroup(
			Filter{Col: "email", Op: FilterOpLike, Val: "kit"},
		),
	), testColumns, nil)

	require.NoError(t, err)
	assert.Contains(t, renderSelect(t, query), `"u"."email" LIKE '%kit%'`)
}

func TestApplyFilterSkipsEmptyChild(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, andGroup(
		Filter{Col: "email", Op: FilterOpExact, Val: "kit"},
		Filter{},   // empty leaf
		andGroup(), // empty group
	), testColumns, nil)

	require.NoError(t, err)
	assert.Contains(t, renderSelect(t, query), `"u"."email" = 'kit'`)
}

func TestApplyFilterCustomExpr(t *testing.T) {
	query := newSelect()
	custom := map[string]FilterExpr{
		"full_name": func(f Filter) (string, []any, error) {
			return "concat_ws(' ', p.first_name, p.last_name) ILIKE ?", []any{"%" + f.Val.(string) + "%"}, nil
		},
	}

	err := ApplyFilter(query, Filter{Col: "full_name", Op: FilterOpLike, Val: "kit"}, testColumns, custom)

	require.NoError(t, err)
	assert.Contains(t, renderSelect(t, query), `concat_ws(' ', p.first_name, p.last_name) ILIKE '%kit%'`)
}

func TestApplyFilterCustomInsideOrGroup(t *testing.T) {
	query := newSelect()
	custom := map[string]FilterExpr{
		"full_name": func(f Filter) (string, []any, error) {
			return "concat(p.first, p.last) ILIKE ?", []any{"%x%"}, nil
		},
	}

	err := ApplyFilter(query, orGroup(
		Filter{Col: "email", Op: FilterOpExact, Val: "a"},
		Filter{Col: "full_name", Op: FilterOpLike, Val: "x"},
	), testColumns, custom)

	require.NoError(t, err)
	sql := renderSelect(t, query)
	assert.Contains(t, sql, `concat(p.first, p.last) ILIKE '%x%'`)
	assert.Contains(t, sql, " OR ")
}

func TestApplyFilterCustomTakesPrecedence(t *testing.T) {
	query := newSelect()
	custom := map[string]FilterExpr{
		"email": func(f Filter) (string, []any, error) {
			return "custom_email(?)", []any{f.Val}, nil
		},
	}

	err := ApplyFilter(query, Filter{Col: "email", Op: FilterOpExact, Val: "kit"}, testColumns, custom)

	require.NoError(t, err)
	sql := renderSelect(t, query)
	assert.Contains(t, sql, `custom_email('kit')`)
	assert.NotContains(t, sql, `"u"."email" =`)
}

func TestApplyFilterErrorFromNestedLeaf(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, andGroup(
		Filter{Col: "email", Op: FilterOpExact, Val: "kit"},
		orGroup(
			Filter{Col: "missing", Op: FilterOpExact, Val: "x"},
		),
	), testColumns, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid filter field "missing"`)
}

func TestApplyFilterCustomExprError(t *testing.T) {
	query := newSelect()
	custom := map[string]FilterExpr{
		"boom": func(f Filter) (string, []any, error) {
			return "", nil, errors.New("orm: custom boom")
		},
	}

	err := ApplyFilter(query, Filter{Col: "boom", Op: FilterOpExact, Val: "x"}, testColumns, custom)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom boom")
}

func TestApplyFilterReturnsInvalidField(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, Filter{Col: "missing", Op: FilterOpExact, Val: "x"}, nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid filter field "missing"`)
}

func TestApplyFilterNullOps(t *testing.T) {
	tests := map[string]struct {
		filter Filter
		want   string
	}{
		"null": {
			filter: Filter{Col: "deleted_at", Op: FilterOpNull},
			want:   `"u"."deleted_at" IS NULL`,
		},
		"not_null": {
			filter: Filter{Col: "deleted_at", Op: FilterOpNotNull},
			want:   `"u"."deleted_at" IS NOT NULL`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			query := newSelect()

			err := ApplyFilter(query, tt.filter, map[string]string{"deleted_at": "u.deleted_at"}, nil)

			require.NoError(t, err)
			assert.Contains(t, renderSelect(t, query), tt.want)
		})
	}
}

func TestApplyFilterReturnsInvalidVals(t *testing.T) {
	tests := map[string]struct {
		filter Filter
		want   string
	}{
		"in_empty": {
			filter: Filter{Col: "status", Op: FilterOpIn},
			want:   `filter op "IN" requires at least one value`,
		},
		"between_one_value": {
			filter: Filter{Col: "age", Op: FilterOpBetween, Vals: []any{"18"}},
			want:   `filter op "BETWEEN" requires exactly two values`,
		},
		"between_exclusive_one_value": {
			filter: Filter{Col: "age", Op: FilterOpBetweenExclusive, Vals: []any{"18"}},
			want:   `filter op "BETWEEN_EXCLUSIVE" requires exactly two values`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			query := newSelect()

			err := ApplyFilter(query, tt.filter, map[string]string{"age": "u.age", "status": "u.status"}, nil)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestApplyFilterReturnsInvalidOp(t *testing.T) {
	query := newSelect()

	err := ApplyFilter(query, Filter{Col: "email", Op: "bad", Val: "x"}, map[string]string{"email": "u.email"}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid filter op "bad"`)
}

func TestFilterFromProtoLeaf(t *testing.T) {
	got := FilterFromProto(testProtoFilter{col: "email", op: testProtoFilterOpLike, val: "kit"})

	assert.Equal(t, Filter{Col: "email", Op: FilterOpLike, Val: "kit", Logic: LogicalOpAnd}, got)
}

func TestFilterFromProtoRecursive(t *testing.T) {
	got := FilterFromProto(testProtoFilter{
		logic: testProtoLogicalOpAnd,
		filters: []testProtoFilter{
			{col: "age", op: testProtoFilterOpGTE, val: "18"},
			{
				logic: testProtoLogicalOpOr,
				filters: []testProtoFilter{
					{col: "status", op: testProtoFilterOpExact, val: "active"},
					{col: "status", op: testProtoFilterOpExact, val: "pending"},
				},
			},
		},
	})

	assert.Equal(t, Filter{
		Logic: LogicalOpAnd,
		Filters: []Filter{
			{Col: "age", Op: FilterOpGTE, Val: "18", Logic: LogicalOpAnd},
			{
				Logic: LogicalOpOr,
				Filters: []Filter{
					{Col: "status", Op: FilterOpExact, Val: "active", Logic: LogicalOpAnd},
					{Col: "status", Op: FilterOpExact, Val: "pending", Logic: LogicalOpAnd},
				},
			},
		},
	}, got)
}

func TestFilterFromProtoInList(t *testing.T) {
	got := FilterFromProto(testProtoFilter{col: "status", op: testProtoFilterOpIn, vals: []string{"active", "pending"}})

	assert.Equal(t, Filter{Col: "status", Op: FilterOpIn, Val: "", Vals: []any{"active", "pending"}, Logic: LogicalOpAnd}, got)
}

func TestFilterOpFromProto(t *testing.T) {
	tests := map[string]struct {
		op   testProtoFilterOp
		want FilterOp
	}{
		"unspecified":       {op: testProtoFilterOpUnspecified, want: FilterOpExact},
		"exact":             {op: testProtoFilterOpExact, want: FilterOpExact},
		"like":              {op: testProtoFilterOpLike, want: FilterOpLike},
		"gt":                {op: testProtoFilterOpGT, want: FilterOpGT},
		"lt":                {op: testProtoFilterOpLT, want: FilterOpLT},
		"gte":               {op: testProtoFilterOpGTE, want: FilterOpGTE},
		"lte":               {op: testProtoFilterOpLTE, want: FilterOpLTE},
		"null":              {op: testProtoFilterOpNull, want: FilterOpNull},
		"not_null":          {op: testProtoFilterOpNotNull, want: FilterOpNotNull},
		"like_ci":           {op: testProtoFilterOpLikeCI, want: FilterOpLikeCI},
		"in":                {op: testProtoFilterOpIn, want: FilterOpIn},
		"between":           {op: testProtoFilterOpBetween, want: FilterOpBetween},
		"between_exclusive": {op: testProtoFilterOpBetweenExclusive, want: FilterOpBetweenExclusive},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, FilterOpFromProto(tt.op))
		})
	}
}

func TestLogicalOpFromProto(t *testing.T) {
	assert.Equal(t, LogicalOpAnd, LogicalOpFromProto(testProtoLogicalOpUnspecified))
	assert.Equal(t, LogicalOpAnd, LogicalOpFromProto(testProtoLogicalOpAnd))
	assert.Equal(t, LogicalOpOr, LogicalOpFromProto(testProtoLogicalOpOr))
}

func TestApplyOrderBy(t *testing.T) {
	query := newSelect()

	err := ApplyOrderBy(query, []OrderBy{{Col: "email", Order: OrderDirectionDesc}}, map[string]string{"email": "u.email"})

	require.NoError(t, err)
	assert.Contains(t, renderSelect(t, query), `ORDER BY "u"."email" DESC`)
}

func TestOrderByFromProto(t *testing.T) {
	got := OrderByFromProto([]testProtoOrderBy{
		{col: "email", order: testProtoOrderDirectionAsc},
		{col: "created_at", order: testProtoOrderDirectionDesc},
	})

	assert.Equal(t, []OrderBy{
		{Col: "email", Order: OrderDirectionAsc},
		{Col: "created_at", Order: OrderDirectionDesc},
	}, got)
}

func TestOrderDirectionFromProto(t *testing.T) {
	assert.Equal(t, OrderDirectionAsc, OrderDirectionFromProto(testProtoOrderDirectionUnspecified))
	assert.Equal(t, OrderDirectionAsc, OrderDirectionFromProto(testProtoOrderDirectionAsc))
	assert.Equal(t, OrderDirectionDesc, OrderDirectionFromProto(testProtoOrderDirectionDesc))
}

func TestApplyOrderByNoopWhenEmpty(t *testing.T) {
	query := newSelect()

	err := ApplyOrderBy(query, nil, map[string]string{"email": "u.email"})

	require.NoError(t, err)
	assert.NotContains(t, renderSelect(t, query), "ORDER BY")
}

func TestApplyOrderByReturnsInvalidField(t *testing.T) {
	query := newSelect()

	err := ApplyOrderBy(query, []OrderBy{{Col: "missing", Order: OrderDirectionAsc}}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid order field "missing"`)
}

func TestApplyOrderByReturnsInvalidDirection(t *testing.T) {
	query := newSelect()

	err := ApplyOrderBy(query, []OrderBy{{Col: "email", Order: "bad"}}, map[string]string{"email": "u.email"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid order direction "bad"`)
}

func TestWritableColumns(t *testing.T) {
	got := WritableColumns(map[string]string{
		"id":         "id",
		"email":      "email",
		"created_at": "created_at",
	}, "id", "created_at")

	assert.Equal(t, map[string]string{"email": "email"}, got)
}

func TestApplyPatchFields(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewUpdate().
		Model((*testPatchUser)(nil)).
		Where("id = ?", "user-id")

	err := ApplyPatchFields(query, map[string]any{"email": "kit@example.com"}, map[string]string{"email": "email"})

	require.NoError(t, err)
	sql, err := query.AppendQuery(query.DB().QueryGen(), nil)
	require.NoError(t, err)
	assert.Contains(t, string(sql), `UPDATE "users" AS "u" SET "email" = 'kit@example.com'`)
	assert.Contains(t, string(sql), `WHERE (id = 'user-id')`)
}

func TestApplyPatchFieldsReturnsInvalidField(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewUpdate().
		Model((*testPatchUser)(nil))

	err := ApplyPatchFields(query, map[string]any{"missing": "x"}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid patch field "missing"`)
}
