package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnake(t *testing.T) {
	tests := map[string]string{
		"UserID":    "user_id",
		"HTTPSPort": "https_port",
		"URLValue":  "url_value",
		"Line1":     "line1",
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, Snake(input))
		})
	}
}

func TestProtoGoName(t *testing.T) {
	cases := map[string]string{
		"id":           "Id",
		"user_id":      "UserId",
		"display_name": "DisplayName",
		"line1":        "Line1",
		"":             "",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, ProtoGoName(input))
		})
	}
}

func TestStructTag(t *testing.T) {
	src := `package p; type T struct { Name string ` + "`bun:\"name,type:text\"`" + ` }`
	file, err := parser.ParseFile(token.NewFileSet(), "test.go", src, parser.ParseComments)
	require.NoError(t, err)
	field := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List[0]
	assert.Equal(t, "name,type:text", StructTag(field).Get("bun"))
}

func TestStructTagWithoutTag(t *testing.T) {
	assert.Equal(t, reflect.StructTag(""), StructTag(&ast.Field{}))
}
