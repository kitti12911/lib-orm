package mappergen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oasFixture builds a minimal huma REST gateway repo: api models, fake domain
// pb.go, and the shared common pb.go (pagination).
func oasFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	write("go.mod", "module demo\n\ngo 1.26.4\n")

	write("internal/api/users/v1/model.go", `package usersv1

import (
	"time"

	userv1 "demo/gen/grpc/user/v1"
)

var _ userv1.User

type User struct {
	ID        string    `+"`json:\"id\"`"+`
	Email     string    `+"`json:\"email\"`"+`
	Status    string    `+"`json:\"status\"`"+`
	Profile   *Profile  `+"`json:\"profile,omitempty\"`"+`
	CreatedAt time.Time `+"`json:\"createdAt\"`"+`
}

type Profile struct {
	FirstName *string `+"`json:\"firstName,omitempty\"`"+`
}

type CreateUserRequest struct {
	Email   string         `+"`json:\"email\"`"+`
	Status  string         `+"`json:\"status\"`"+`
	Profile *CreateProfile `+"`json:\"profile,omitempty\"`"+`
}

type CreateProfile struct {
	FirstName *string `+"`json:\"firstName,omitempty\"`"+`
}

type ListUsersOutput struct {
	Body UserList
}

type UserList struct {
	Users     []User `+"`json:\"users\"`"+`
	Page      int    `+"`json:\"page\"`"+`
	TotalSize int    `+"`json:\"totalSize\"`"+`
}

type CreateUserOutput struct {
	Body CreateUserResult
}

type CreateUserResult struct {
	ID string `+"`json:\"id\"`"+`
}

type GetUserOutput struct {
	Body User
}
`)

	write("gen/grpc/user/v1/user.pb.go", `package userv1

import (
	v1 "demo/gen/grpc/common/v1"

	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

type UserStatus int32

const (
	UserStatus_USER_STATUS_UNSPECIFIED UserStatus = 0
	UserStatus_USER_STATUS_ACTIVE      UserStatus = 1
)

type User struct {
	Id        string
	Email     string
	Status    UserStatus
	Profile   *UserProfile
	CreatedAt *timestamppb.Timestamp
}

type UserProfile struct {
	FirstName *string
}

type CreateUserRequest struct {
	User *User
}

type ListUsersResponse struct {
	Users      []*User
	Pagination *v1.PaginationResponse
}

type CreateUserResponse struct {
	Id string
}

type GetUserResponse struct {
	User *User
}
`)

	write("gen/grpc/common/v1/pagination.pb.go", `package commonv1

type PaginationResponse struct {
	Page      int32
	PageSize  int32
	TotalSize int32
}
`)

	return dir
}

func oasGenerated(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "internal", "api", "users", "v1", "mapper_generated.go"))
	require.NoError(t, err)
	return string(data)
}

func TestOASGeneratesModelMappers(t *testing.T) {
	dir := oasFixture(t)

	require.NoError(t, Run([]string{"-C", dir}))
	src := oasGenerated(t, dir)

	// response models: root by value, nested by pointer
	assert.Contains(t, src, "func userFromProto(in *userv1.User) User {")
	assert.Contains(t, src, "func profileFromProto(in *userv1.UserProfile) *Profile {")
	assert.Contains(t, src, "CreatedAt: protoutil.TimeFromProto(in.CreatedAt),")
	assert.Contains(t, src, "Status:    userStatusFromProto(in.Status),")
	assert.Contains(t, src, "Profile:   profileFromProto(in.Profile),")

	// request direction: root request by value, nested by pointer
	assert.Contains(t, src, "func userToProto(src CreateUserRequest) *userv1.User {")
	assert.Contains(t, src, "func profileToProto(src *CreateProfile) *userv1.UserProfile {")
	assert.Contains(t, src, "Status:  toProtoUserStatus(src.Status),")

	// enum bridges generated from the proto enum
	assert.Contains(t, src, "func toProtoUserStatus(value string) userv1.UserStatus {")
	assert.Contains(t, src, "func userStatusFromProto(value userv1.UserStatus) string {")
}

func TestOASGeneratesEnvelopes(t *testing.T) {
	dir := oasFixture(t)

	require.NoError(t, Run([]string{"-C", dir}))
	src := oasGenerated(t, dir)

	// list envelope: loop + pagination flattening verified against common/v1
	assert.Contains(t, src, "func listUsersOutputFromProto(in *userv1.ListUsersResponse) *ListUsersOutput {")
	assert.Contains(t, src, "users := make([]User, 0, len(in.GetUsers()))")
	assert.Contains(t, src, "users = append(users, userFromProto(item))")
	assert.Contains(t, src, "Page:      int(in.GetPagination().GetPage()),")
	assert.Contains(t, src, "TotalSize: int(in.GetPagination().GetTotalSize()),")

	// scalar-result envelope
	assert.Contains(t, src, "func createUserOutputFromProto(in *userv1.CreateUserResponse) *CreateUserOutput {")
	assert.Contains(t, src, "ID: in.GetId(),")

	// whole-model Body envelope
	assert.Contains(t, src, "func getUserOutputFromProto(in *userv1.GetUserResponse) *GetUserOutput {")
	assert.Contains(t, src, "Body: userFromProto(in.GetUser()),")
}

func TestOASIdempotent(t *testing.T) {
	dir := oasFixture(t)
	require.NoError(t, Run([]string{"-C", dir}))
	first := oasGenerated(t, dir)
	require.NoError(t, Run([]string{"-C", dir}))
	assert.Equal(t, first, oasGenerated(t, dir))
}

func TestOASHonorsIgnore(t *testing.T) {
	dir := oasFixture(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "internal", "api", "users", "v1", "doc.go"),
		[]byte("//mapgen:ignore\npackage usersv1\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "api", "users", "v1", "mapper_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestOASSkipsDomainWithoutProtoImport(t *testing.T) {
	dir := oasFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "api", "system", "v1"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "internal", "api", "system", "v1", "model.go"),
		[]byte("package systemv1\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "api", "system", "v1", "mapper_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestOASSkipsDomainWithoutRootModel(t *testing.T) {
	dir := oasFixture(t)
	// A domain whose proto has no message named after the domain root.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "api", "worker", "v1"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "internal", "api", "worker", "v1", "model.go"),
		[]byte("package workerv1\n\nimport workerpb \"demo/gen/grpc/worker/v1\"\n\nvar _ = workerpb.SubmitJobRequest{}\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "gen", "grpc", "worker", "v1"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "gen", "grpc", "worker", "v1", "worker.pb.go"),
		[]byte("package workerv1\n\ntype SubmitJobRequest struct {\n\tId string\n}\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "api", "worker", "v1", "mapper_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestOASErrors(t *testing.T) {
	t.Run("missing api dir handled by grpc mode", func(t *testing.T) {
		// Without internal/api the run falls through to grpc mode and fails on
		// the missing model dir — covered by grpc-mode tests; nothing here.
	})

	t.Run("parse error in api package", func(t *testing.T) {
		dir := oasFixture(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "internal", "api", "users", "v1", "bad.go"),
			[]byte("package usersv1\ntype Broken struct {"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})

	t.Run("proto parse error", func(t *testing.T) {
		dir := oasFixture(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "gen", "grpc", "user", "v1", "bad.pb.go"),
			[]byte("package userv1\ntype Broken struct {"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})
}
