package orm

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"
)

// UUID is a dialect-portable UUID column rendered as its canonical lowercase
// string. It scans SQL Server uniqueidentifier bytes and Postgres uuid/text.
// The zero value is the empty string, and Value encodes it as NULL so database
// defaults can generate IDs on insert.
type UUID string

func (u UUID) String() string { return string(u) }

func (u UUID) Value() (driver.Value, error) {
	if u == "" {
		return nil, nil
	}
	id, err := uuid.Parse(string(u))
	if err != nil {
		return nil, fmt.Errorf("orm: invalid uuid %q: %w", string(u), err)
	}
	return id.String(), nil
}

func (u *UUID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*u = ""
		return nil
	case string:
		return u.scanString(v)
	case []byte:
		if len(v) == 16 {
			var raw mssql.UniqueIdentifier
			if err := raw.Scan(v); err != nil {
				return fmt.Errorf("orm: scan uniqueidentifier: %w", err)
			}
			*u = UUID(strings.ToLower(raw.String()))
			return nil
		}
		return u.scanString(string(v))
	default:
		return fmt.Errorf("orm: cannot scan %T into UUID", src)
	}
}

func (u *UUID) scanString(s string) error {
	if s == "" {
		*u = ""
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return fmt.Errorf("orm: invalid uuid %q: %w", s, err)
	}
	*u = UUID(id.String())
	return nil
}
