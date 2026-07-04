package mappergen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	write("go.mod", "module demo\n\ngo 1.26.4\n")

	write("internal/database/model.go", `package database

import (
	"database/sql"
	"time"

	"github.com/uptrace/bun"
)

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	ID          string       `+"`bun:\"id,pk\"`"+`
	Email       string       `+"`bun:\"email\"`"+`
	DisplayName *string      `+"`bun:\"display_name\"`"+`
	Status      string       `+"`bun:\"status\"`"+`
	CreatedAt   time.Time    `+"`bun:\"created_at\"`"+`
	DeletedAt   sql.NullTime `+"`bun:\"deleted_at\"`"+`
	Profile     *UserProfile `+"`bun:\"rel:has-one,join:id=user_id\"`"+`
}

type UserProfile struct {
	bun.BaseModel `+"`bun:\"table:user_profiles,alias:up\"`"+`
	ID        string       `+"`bun:\"id,pk\"`"+`
	FirstName *string      `+"`bun:\"first_name\"`"+`
	User      *User        `+"`bun:\"rel:belongs-to,join:user_id=id\"`"+`
	Address   *UserAddress `+"`bun:\"rel:has-one,join:id=user_profile_id\"`"+`
}

type UserAddress struct {
	bun.BaseModel `+"`bun:\"table:user_addresses,alias:ua\"`"+`
	ID   string  `+"`bun:\"id,pk\"`"+`
	City *string `+"`bun:\"city\"`"+`
}
`)

	write("internal/feature/user/user.go", `package user

type CreateParams struct {
	Email       string               `+"`field:\"email\"`"+`
	DisplayName *string              `+"`field:\"display_name\"`"+`
	Status      string               `+"`field:\"status\"`"+`
	Profile     *CreateProfileParams `+"`field:\"profile\"`"+`
}

type CreateProfileParams struct {
	FirstName *string              `+"`field:\"first_name\"`"+`
	Address   *CreateAddressParams `+"`field:\"address\"`"+`
}

type CreateAddressParams struct {
	City *string `+"`field:\"city\"`"+`
}
`)

	write("gen/grpc/user/v1/user.pb.go", `package userv1

import timestamppb "google.golang.org/protobuf/types/known/timestamppb"

type UserStatus int32

const (
	UserStatus_USER_STATUS_UNSPECIFIED UserStatus = 0
	UserStatus_USER_STATUS_ACTIVE      UserStatus = 1
	UserStatus_USER_STATUS_DISABLED    UserStatus = 2
)

type User struct {
	state         int
	Id            string
	Email         string
	DisplayName   *string
	Status        UserStatus
	Profile       *UserProfile
	CreatedAt     *timestamppb.Timestamp
	unknownFields int
}

type UserProfile struct {
	FirstName *string
	Address   *UserAddress
}

type UserAddress struct {
	City *string
}
`)

	return dir
}

func generated(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "internal", "feature", "user", "mapper_generated.go"))
	require.NoError(t, err)
	return string(data)
}

func TestRunGeneratesMappers(t *testing.T) {
	dir := fixture(t)

	require.NoError(t, Run([]string{"-C", dir}))
	src := generated(t, dir)

	// to_proto: root + reachable relation models
	assert.Contains(t, src, "func toProtoUser(src *database.User) *userv1.User {")
	assert.Contains(t, src, "func toProtoUserProfile(src *database.UserProfile) *userv1.UserProfile {")
	assert.Contains(t, src, "func toProtoUserAddress(src *database.UserAddress) *userv1.UserAddress {")

	// field intersection: matched fields present, DeletedAt (no proto field) absent
	assert.Contains(t, src, "Id:          src.ID,")
	assert.Contains(t, src, "CreatedAt:   timestamppb.New(src.CreatedAt),")
	assert.NotContains(t, src, "DeletedAt")

	// nested wiring + enum bridge call
	assert.Contains(t, src, "Profile:     toProtoUserProfile(src.Profile),")
	assert.Contains(t, src, "Status:      toProtoUserStatus(src.Status),")

	// from_proto: root returns value, nested returns pointer
	assert.Contains(t, src, "func createParamsFromProto(in *userv1.User) CreateParams {")
	assert.Contains(t, src, "return CreateParams{}")
	assert.Contains(t, src, "func createProfileParamsFromProto(in *userv1.UserProfile) *CreateProfileParams {")
	assert.Contains(t, src, "Status:      userStatusFromProto(in.Status),")
	assert.Contains(t, src, "Profile:     createProfileParamsFromProto(in.Profile),")

	// auto-generated enum bridges from the proto enum
	assert.Contains(t, src, "func toProtoUserStatus(value string) userv1.UserStatus {")
	assert.Contains(t, src, `case "active":`)
	assert.Contains(t, src, "return userv1.UserStatus_USER_STATUS_ACTIVE")
	assert.Contains(t, src, "func userStatusFromProto(value userv1.UserStatus) string {")
	assert.Contains(t, src, "return userv1.UserStatus_USER_STATUS_UNSPECIFIED")

	// import block
	assert.Contains(t, src, `userv1 "demo/gen/grpc/user/v1"`)
	assert.Contains(t, src, `database "demo/internal/database"`)
	assert.Contains(t, src, `"google.golang.org/protobuf/types/known/timestamppb"`)
}

func TestRunIdempotent(t *testing.T) {
	dir := fixture(t)
	require.NoError(t, Run([]string{"-C", dir}))
	first := generated(t, dir)
	require.NoError(t, Run([]string{"-C", dir}))
	assert.Equal(t, first, generated(t, dir))
}

func TestRunHonorsIgnore(t *testing.T) {
	dir := fixture(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "internal", "feature", "user", "doc.go"),
		[]byte("//mapgen:ignore\npackage user\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "feature", "user", "mapper_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestRunProtoOverride(t *testing.T) {
	dir := fixture(t)
	// Rename the proto message and point the params struct at it via directive.
	pb := filepath.Join(dir, "gen", "grpc", "user", "v1", "user.pb.go")
	data, err := os.ReadFile(pb) //nolint:gosec // test path under t.TempDir()
	require.NoError(t, err)
	//nolint:gosec // test path under t.TempDir()
	require.NoError(t, os.WriteFile(pb, append(data, []byte("\ntype Account struct {\n\tEmail string\n}\n")...), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "internal", "feature", "user", "extra.go"),
		[]byte("package user\n\n//mapgen:proto=Account\ntype CreateAccountParams struct {\n\tEmail string `field:\"email\"`\n}\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))
	src := generated(t, dir)
	assert.Contains(t, src, "func createAccountParamsFromProto(in *userv1.Account) *CreateAccountParams {")
}

func TestRunSkipsFeatureWithoutProto(t *testing.T) {
	dir := fixture(t)
	// A feature dir with no gen/grpc/<f>/v1 package is skipped.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "feature", "billing"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "internal", "feature", "billing", "b.go"), []byte("package billing\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))
	_, err := os.Stat(filepath.Join(dir, "internal", "feature", "billing", "mapper_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestRunErrors(t *testing.T) {
	t.Run("flag parse error", func(t *testing.T) {
		assert.Error(t, Run([]string{"-nope"}))
	})
	t.Run("missing go.mod", func(t *testing.T) {
		assert.Error(t, Run([]string{"-C", t.TempDir()}))
	})
	t.Run("missing model dir", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})
}
