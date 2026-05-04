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

type Filter struct {
	Col  string
	Op   FilterOp
	Val  any
	Vals []any
}

type ProtoFilter[O ~int32] interface {
	GetCol() string
	GetOp() O
	GetVal() string
	GetVals() []string
}

type ProtoOrderBy[O ~int32] interface {
	GetCol() string
	GetOrder() O
}

func FiltersFromProto[T ProtoFilter[O], O ~int32](filters []T) []Filter {
	result := make([]Filter, len(filters))
	for i, filter := range filters {
		result[i] = Filter{
			Col:  filter.GetCol(),
			Op:   FilterOpFromProto(filter.GetOp()),
			Val:  filter.GetVal(),
			Vals: stringsToAny(filter.GetVals()),
		}
	}
	return result
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

func OrderDirectionFromProto[O ~int32](direction O) OrderDirection {
	if int32(direction) == 2 {
		return OrderDirectionDesc
	}
	return OrderDirectionAsc
}

func ApplyFilters(query *bun.SelectQuery, filters []Filter, columns map[string]string) error {
	for _, filter := range filters {
		column, ok := columns[filter.Col]
		if !ok {
			return fmt.Errorf("orm: invalid filter field %q", filter.Col)
		}

		switch filter.Op {
		case FilterOpLike:
			query.Where("? LIKE ?", bun.Ident(column), "%"+fmt.Sprint(filter.Val)+"%")
		case FilterOpLikeCI:
			query.Where("LOWER(?) LIKE LOWER(?)", bun.Ident(column), "%"+fmt.Sprint(filter.Val)+"%")
		case FilterOpIn:
			if len(filter.Vals) == 0 {
				return fmt.Errorf("orm: filter op %q requires at least one value", filter.Op)
			}
			query.Where("? IN (?)", bun.Ident(column), bun.List(filter.Vals))
		case FilterOpBetween:
			if len(filter.Vals) != 2 {
				return fmt.Errorf("orm: filter op %q requires exactly two values", filter.Op)
			}
			query.Where("? BETWEEN ? AND ?", bun.Ident(column), filter.Vals[0], filter.Vals[1])
		case FilterOpBetweenExclusive:
			if len(filter.Vals) != 2 {
				return fmt.Errorf("orm: filter op %q requires exactly two values", filter.Op)
			}
			query.Where("? > ? AND ? < ?", bun.Ident(column), filter.Vals[0], bun.Ident(column), filter.Vals[1])
		case FilterOpNull:
			query.Where("? IS NULL", bun.Ident(column))
		case FilterOpNotNull:
			query.Where("? IS NOT NULL", bun.Ident(column))
		case FilterOpExact, FilterOpGT, FilterOpLT, FilterOpGTE, FilterOpLTE:
			query.Where("? "+string(filter.Op)+" ?", bun.Ident(column), filter.Val)
		default:
			return fmt.Errorf("orm: invalid filter op %q", filter.Op)
		}
	}

	return nil
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
