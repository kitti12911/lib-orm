package codegen

import (
	"go/ast"
	"reflect"
	"strings"
	"unicode"
)

// Snake converts a Go identifier to snake_case, treating runs of capitals as
// initialisms: "UserID" -> "user_id", "HTTPSPort" -> "https_port",
// "URLValue" -> "url_value", "Line1" -> "line1".
func Snake(name string) string {
	var out []rune
	runes := []rune(name)
	for i, r := range runes {
		if i > 0 && shouldSplitWord(runes, i) {
			out = append(out, '_')
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

func shouldSplitWord(runes []rune, i int) bool {
	prev := runes[i-1]
	curr := runes[i]

	if unicode.IsLower(prev) && unicode.IsUpper(curr) {
		return true
	}
	if unicode.IsUpper(prev) && unicode.IsUpper(curr) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
		return true
	}
	return false
}

// ProtoGoName converts a snake_case proto field name to the Go identifier
// protoc-gen-go would emit: title-case each underscore-delimited word.
// "user_id" -> "UserId", "display_name" -> "DisplayName".
func ProtoGoName(column string) string {
	parts := strings.Split(column, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// StructTag returns the parsed struct tag of an AST field, or the empty tag
// when the field carries none.
func StructTag(f *ast.Field) reflect.StructTag {
	if f.Tag == nil {
		return ""
	}
	return reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
}
