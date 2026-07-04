package orm

import (
	"fmt"

	"github.com/uptrace/bun"
)

type OrderDirection string

const (
	OrderDirectionAsc  OrderDirection = "ASC"
	OrderDirectionDesc OrderDirection = "DESC"
)

type OrderBy struct {
	Col   string
	Order OrderDirection
}

type FilterOp string

const (
	FilterOpExact            FilterOp = "="
	FilterOpLike             FilterOp = "LIKE"
	FilterOpGT               FilterOp = ">"
	FilterOpLT               FilterOp = "<"
	FilterOpGTE              FilterOp = ">="
	FilterOpLTE              FilterOp = "<="
	FilterOpNull             FilterOp = "NULL"
	FilterOpNotNull          FilterOp = "NOT_NULL"
	FilterOpLikeCI           FilterOp = "LIKE_CI"
	FilterOpIn               FilterOp = "IN"
	FilterOpBetween          FilterOp = "BETWEEN"
	FilterOpBetweenExclusive FilterOp = "BETWEEN_EXCLUSIVE"
)

type LogicalOp string

const (
	LogicalOpAnd LogicalOp = "AND"
	LogicalOpOr  LogicalOp = "OR"
)

// Filter is a recursive predicate. A leaf carries Col/Op/Val(s). A group carries
// Logic + Filters (non-empty Filters ⇒ group; Col/Op/Val(s) ignored). An empty
// node (no Col, no children) is a no-op.
type Filter struct {
	Col     string
	Op      FilterOp
	Val     any
	Vals    []any
	Logic   LogicalOp
	Filters []Filter
}

func (f Filter) isGroup() bool { return len(f.Filters) > 0 }

func (f Filter) isEmpty() bool { return f.Col == "" && len(f.Filters) == 0 }

// FilterExpr builds a raw SQL expression + args for a single leaf, letting a
// service back a virtual/computed column (subquery, EXISTS, expression) that has
// no physical column. Registered per column in the custom map passed to
// ApplyFilter; returning expr+args (not mutating the query) lets a custom leaf
// compose correctly inside AND/OR groups at any depth.
type FilterExpr func(f Filter) (expr string, args []any, err error)

type ProtoFilter[T any, O ~int32, L ~int32] interface {
	GetCol() string
	GetOp() O
	GetVal() string
	GetVals() []string
	GetLogic() L
	GetFilters() []T
}

type ProtoOrderBy[O ~int32] interface {
	GetCol() string
	GetOrder() O
}

// FilterFromProto converts a proto Filter tree to an orm.Filter tree. A typed-nil
// proto message yields an empty (no-op) Filter, since protoc getters are nil-safe.
func FilterFromProto[T ProtoFilter[T, O, L], O ~int32, L ~int32](f T) Filter {
	children := f.GetFilters()
	if len(children) > 0 {
		out := Filter{
			Logic:   LogicalOpFromProto(f.GetLogic()),
			Filters: make([]Filter, 0, len(children)),
		}
		for _, child := range children {
			out.Filters = append(out.Filters, FilterFromProto[T, O, L](child))
		}
		return out
	}

	return Filter{
		Col:   f.GetCol(),
		Op:    FilterOpFromProto(f.GetOp()),
		Val:   f.GetVal(),
		Vals:  stringsToAny(f.GetVals()),
		Logic: LogicalOpFromProto(f.GetLogic()),
	}
}

func OrderByFromProto[T ProtoOrderBy[O], O ~int32](orderBy []T) []OrderBy {
	result := make([]OrderBy, len(orderBy))
	for i, order := range orderBy {
		result[i] = OrderBy{
			Col:   order.GetCol(),
			Order: OrderDirectionFromProto(order.GetOrder()),
		}
	}
	return result
}

func FilterOpFromProto[O ~int32](op O) FilterOp {
	switch int32(op) {
	case 2:
		return FilterOpLike
	case 3:
		return FilterOpGT
	case 4:
		return FilterOpLT
	case 5:
		return FilterOpGTE
	case 6:
		return FilterOpLTE
	case 7:
		return FilterOpNull
	case 8:
		return FilterOpNotNull
	case 9:
		return FilterOpLikeCI
	case 10:
		return FilterOpIn
	case 11:
		return FilterOpBetween
	case 12:
		return FilterOpBetweenExclusive
	default:
		return FilterOpExact
	}
}

func LogicalOpFromProto[L ~int32](op L) LogicalOp {
	if int32(op) == 2 {
		return LogicalOpOr
	}
	return LogicalOpAnd
}

func OrderDirectionFromProto[O ~int32](direction O) OrderDirection {
	if int32(direction) == 2 {
		return OrderDirectionDesc
	}
	return OrderDirectionAsc
}

// ApplyFilter applies a recursive filter tree to a select query. columns gates
// physical fields (name → column); custom (may be nil) backs virtual fields.
func ApplyFilter(query *bun.SelectQuery, f Filter, columns map[string]string, custom map[string]FilterExpr) error {
	if f.isEmpty() {
		return nil
	}

	var err error
	query.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
		if f.isGroup() {
			err = applyFilterMembers(q, f, columns, custom)
			return q
		}
		var (
			expr string
			args []any
		)
		expr, args, err = leafExpr(f, columns, custom)
		if err == nil {
			q.Where(expr, args...)
		}
		return q
	})
	return err
}

func applyFilterMembers(q *bun.SelectQuery, group Filter, columns map[string]string, custom map[string]FilterExpr) error {
	or := group.Logic == LogicalOpOr
	added := 0

	for i := range group.Filters {
		child := group.Filters[i]
		if child.isEmpty() {
			continue
		}

		if child.isGroup() {
			sep := " AND "
			if or && added > 0 {
				sep = " OR "
			}
			var childErr error
			q.WhereGroup(sep, func(sub *bun.SelectQuery) *bun.SelectQuery {
				childErr = applyFilterMembers(sub, child, columns, custom)
				return sub
			})
			if childErr != nil {
				return childErr
			}
			added++
			continue
		}

		expr, args, err := leafExpr(child, columns, custom)
		if err != nil {
			return err
		}
		if or && added > 0 {
			q.WhereOr(expr, args...)
		} else {
			q.Where(expr, args...)
		}
		added++
	}

	return nil
}

func leafExpr(f Filter, columns map[string]string, custom map[string]FilterExpr) (expr string, args []any, err error) {
	if fn, ok := custom[f.Col]; ok {
		return fn(f)
	}

	column, ok := columns[f.Col]
	if !ok {
		return "", nil, fmt.Errorf("orm: invalid filter field %q", f.Col)
	}
	ident := bun.Ident(column)

	switch f.Op {
	case FilterOpLike:
		return "? LIKE ?", []any{ident, "%" + fmt.Sprint(f.Val) + "%"}, nil
	case FilterOpLikeCI:
		return "LOWER(?) LIKE LOWER(?)", []any{ident, "%" + fmt.Sprint(f.Val) + "%"}, nil
	case FilterOpIn:
		if len(f.Vals) == 0 {
			return "", nil, fmt.Errorf("orm: filter op %q requires at least one value", f.Op)
		}
		return "? IN (?)", []any{ident, bun.List(f.Vals)}, nil
	case FilterOpBetween:
		if len(f.Vals) != 2 {
			return "", nil, fmt.Errorf("orm: filter op %q requires exactly two values", f.Op)
		}
		return "? BETWEEN ? AND ?", []any{ident, f.Vals[0], f.Vals[1]}, nil
	case FilterOpBetweenExclusive:
		if len(f.Vals) != 2 {
			return "", nil, fmt.Errorf("orm: filter op %q requires exactly two values", f.Op)
		}
		return "? > ? AND ? < ?", []any{ident, f.Vals[0], ident, f.Vals[1]}, nil
	case FilterOpNull:
		return "? IS NULL", []any{ident}, nil
	case FilterOpNotNull:
		return "? IS NOT NULL", []any{ident}, nil
	case FilterOpExact, FilterOpGT, FilterOpLT, FilterOpGTE, FilterOpLTE:
		return "? " + string(f.Op) + " ?", []any{ident, f.Val}, nil
	default:
		return "", nil, fmt.Errorf("orm: invalid filter op %q", f.Op)
	}
}

func stringsToAny(vals []string) []any {
	if len(vals) == 0 {
		return nil
	}

	result := make([]any, len(vals))
	for i, val := range vals {
		result[i] = val
	}
	return result
}

func ApplyOrderBy(query *bun.SelectQuery, orderBy []OrderBy, columns map[string]string) error {
	for _, order := range orderBy {
		column, ok := columns[order.Col]
		if !ok {
			return fmt.Errorf("orm: invalid order field %q", order.Col)
		}
		if order.Order != OrderDirectionAsc && order.Order != OrderDirectionDesc {
			return fmt.Errorf("orm: invalid order direction %q", order.Order)
		}

		query.OrderExpr("? "+string(order.Order), bun.Ident(column))
	}

	return nil
}

func WritableColumns(fields map[string]string, blocked ...string) map[string]string {
	blockedSet := make(map[string]bool, len(blocked))
	for _, field := range blocked {
		blockedSet[field] = true
	}

	columns := make(map[string]string, max(0, len(fields)-len(blockedSet)))
	for field, column := range fields {
		if blockedSet[field] {
			continue
		}
		columns[field] = column
	}
	return columns
}

func ApplyPatchFields(query *bun.UpdateQuery, fields map[string]any, columns map[string]string) error {
	for field, value := range fields {
		column, ok := columns[field]
		if !ok {
			return fmt.Errorf("orm: invalid patch field %q", field)
		}
		query.Set("? = ?", bun.Ident(column), value)
	}
	return nil
}
