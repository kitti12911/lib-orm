package orm

import (
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

type testProtoOrderDirection int32

const (
	testProtoOrderDirectionUnspecified testProtoOrderDirection = 0
	testProtoOrderDirectionAsc         testProtoOrderDirection = 1
	testProtoOrderDirectionDesc        testProtoOrderDirection = 2
)

type testProtoFilter struct {
	col  string
	op   testProtoFilterOp
	val  string
	vals []string
}

func (f testProtoFilter) GetCol() string           { return f.col }
func (f testProtoFilter) GetOp() testProtoFilterOp { return f.op }
func (f testProtoFilter) GetVal() string           { return f.val }
func (f testProtoFilter) GetVals() []string        { return f.vals }

type testProtoOrderBy struct {
	col   string
	order testProtoOrderDirection
}

func (o testProtoOrderBy) GetCol() string                    { return o.col }
func (o testProtoOrderBy) GetOrder() testProtoOrderDirection { return o.order }

func TestApplyFilters(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()
	columns := map[string]string{"email": "u.email", "age": "u.age", "status": "u.status", "created_at": "u.created_at"}

	err := ApplyFilters(query, []Filter{
		{Col: "email", Op: FilterOpLike, Val: "kit"},
		{Col: "email", Op: FilterOpLikeCI, Val: "Kit"},
		{Col: "age", Op: FilterOpGTE, Val: "18"},
		{Col: "status", Op: FilterOpIn, Vals: []any{"active", "pending"}},
		{Col: "created_at", Op: FilterOpBetween, Vals: []any{"2026-01-01", "2026-01-31"}},
		{Col: "age", Op: FilterOpBetweenExclusive, Vals: []any{"18", "60"}},
		{Col: "email", Op: FilterOpNotNull},
	}, columns)

	require.NoError(t, err)
	sql, err := query.AppendQuery(query.DB().QueryGen(), nil)
	require.NoError(t, err)
	assert.Contains(t, string(sql), `"u"."email" LIKE '%kit%'`)
	assert.Contains(t, string(sql), `LOWER("u"."email") LIKE LOWER('%Kit%')`)
	assert.Contains(t, string(sql), `"u"."age" >= '18'`)
	assert.Contains(t, string(sql), `"u"."status" IN ('active', 'pending')`)
	assert.Contains(t, string(sql), `"u"."created_at" BETWEEN '2026-01-01' AND '2026-01-31'`)
	assert.Contains(t, string(sql), `"u"."age" > '18' AND "u"."age" < '60'`)
	assert.Contains(t, string(sql), `"u"."email" IS NOT NULL`)
}

func TestFiltersFromProto(t *testing.T) {
	got := FiltersFromProto([]testProtoFilter{
		{col: "email", op: testProtoFilterOpLike, val: "kit"},
		{col: "age", op: testProtoFilterOpGTE, val: "18"},
		{col: "status", op: testProtoFilterOpIn, vals: []string{"active", "pending"}},
		{col: "deleted_at", op: testProtoFilterOpNull},
	})

	assert.Equal(t, []Filter{
		{Col: "email", Op: FilterOpLike, Val: "kit"},
		{Col: "age", Op: FilterOpGTE, Val: "18"},
		{Col: "status", Op: FilterOpIn, Val: "", Vals: []any{"active", "pending"}},
		{Col: "deleted_at", Op: FilterOpNull, Val: ""},
	}, got)
}

func TestFilterOpFromProto(t *testing.T) {
	tests := map[string]struct {
		op   testProtoFilterOp
		want FilterOp
	}{
		"unspecified": {op: testProtoFilterOpUnspecified, want: FilterOpExact},
		"exact":       {op: testProtoFilterOpExact, want: FilterOpExact},
		"like":        {op: testProtoFilterOpLike, want: FilterOpLike},
		"gt":          {op: testProtoFilterOpGT, want: FilterOpGT},
		"lt":          {op: testProtoFilterOpLT, want: FilterOpLT},
		"gte":         {op: testProtoFilterOpGTE, want: FilterOpGTE},
		"lte":         {op: testProtoFilterOpLTE, want: FilterOpLTE},
		"null":        {op: testProtoFilterOpNull, want: FilterOpNull},
		"not_null":    {op: testProtoFilterOpNotNull, want: FilterOpNotNull},
		"like_ci":     {op: testProtoFilterOpLikeCI, want: FilterOpLikeCI},
		"in":          {op: testProtoFilterOpIn, want: FilterOpIn},
		"between":     {op: testProtoFilterOpBetween, want: FilterOpBetween},
		"between_exclusive": {
			op:   testProtoFilterOpBetweenExclusive,
			want: FilterOpBetweenExclusive,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, FilterOpFromProto(tt.op))
		})
	}
}

func TestApplyFiltersReturnsInvalidField(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()

	err := ApplyFilters(query, []Filter{{Col: "missing", Op: FilterOpExact, Val: "x"}}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid filter field "missing"`)
}

func TestApplyFiltersNullOps(t *testing.T) {
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
			query := bun.NewDB(nil, pgdialect.New()).NewSelect()

			err := ApplyFilters(query, []Filter{tt.filter}, map[string]string{"deleted_at": "u.deleted_at"})

			require.NoError(t, err)
			sql, err := query.AppendQuery(query.DB().QueryGen(), nil)
			require.NoError(t, err)
			assert.Contains(t, string(sql), tt.want)
		})
	}
}

func TestApplyFiltersReturnsInvalidVals(t *testing.T) {
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
			query := bun.NewDB(nil, pgdialect.New()).NewSelect()

			err := ApplyFilters(query, []Filter{tt.filter}, map[string]string{"age": "u.age", "status": "u.status"})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestApplyFiltersReturnsInvalidOp(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()

	err := ApplyFilters(query, []Filter{{Col: "email", Op: "bad", Val: "x"}}, map[string]string{"email": "u.email"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid filter op "bad"`)
}

func TestApplyOrderBy(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()

	err := ApplyOrderBy(query, []OrderBy{{Col: "email", Order: OrderDirectionDesc}}, map[string]string{"email": "u.email"})

	require.NoError(t, err)
	sql, err := query.AppendQuery(query.DB().QueryGen(), nil)
	require.NoError(t, err)
	assert.Contains(t, string(sql), `ORDER BY "u"."email" DESC`)
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
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()

	err := ApplyOrderBy(query, nil, map[string]string{"email": "u.email"})

	require.NoError(t, err)
	sql, err := query.AppendQuery(query.DB().QueryGen(), nil)
	require.NoError(t, err)
	assert.NotContains(t, string(sql), "ORDER BY")
}

func TestApplyOrderByReturnsInvalidField(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()

	err := ApplyOrderBy(query, []OrderBy{{Col: "missing", Order: OrderDirectionAsc}}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid order field "missing"`)
}

func TestApplyOrderByReturnsInvalidDirection(t *testing.T) {
	query := bun.NewDB(nil, pgdialect.New()).NewSelect()

	err := ApplyOrderBy(query, []OrderBy{{Col: "email", Order: "bad"}}, map[string]string{"email": "u.email"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid order direction "bad"`)
}
