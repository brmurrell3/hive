// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package auth

import (
	"log/slog"
	"os"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestAuthenticate_ValidToken(t *testing.T) {
	t.Parallel()

	rawToken := "test-secret-token-12345"
	hash := HashToken(rawToken)

	users := []User{
		{
			ID:        "alice",
			Name:      "Alice",
			Role:      RoleAdmin,
			TokenHash: hash,
		},
	}

	authorizer := NewAuthorizer(users, testLogger())

	user, err := authorizer.Authenticate(rawToken)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if user == nil {
		t.Fatal("expected user, got nil")
	}
	if user.ID != "alice" {
		t.Errorf("expected user ID 'alice', got %q", user.ID)
	}
	if user.Role != RoleAdmin {
		t.Errorf("expected role admin, got %q", user.Role)
	}
}

func TestAuthenticate_InvalidToken(t *testing.T) {
	t.Parallel()

	users := []User{
		{
			ID:        "alice",
			Role:      RoleAdmin,
			TokenHash: HashToken("correct-token"),
		},
	}

	authorizer := NewAuthorizer(users, testLogger())

	_, err := authorizer.Authenticate("wrong-token")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestAuthorize_ViewerCanListButNotStop(t *testing.T) {
	t.Parallel()

	viewer := &User{
		ID:   "viewer-bob",
		Role: RoleViewer,
	}

	authorizer := NewAuthorizer(nil, testLogger())

	// Viewer should be able to list.
	if err := authorizer.Authorize(viewer, "list", ""); err != nil {
		t.Errorf("viewer should be allowed to list, got error: %v", err)
	}

	// Viewer should be able to get status.
	if err := authorizer.Authorize(viewer, "status", ""); err != nil {
		t.Errorf("viewer should be allowed to get status, got error: %v", err)
	}

	// Viewer should be able to view logs.
	if err := authorizer.Authorize(viewer, "logs", ""); err != nil {
		t.Errorf("viewer should be allowed to view logs, got error: %v", err)
	}

	// Viewer should NOT be able to stop.
	if err := authorizer.Authorize(viewer, "stop", "agent-1"); err == nil {
		t.Error("viewer should NOT be allowed to stop, got nil error")
	}

	// Viewer should NOT be able to start.
	if err := authorizer.Authorize(viewer, "start", "agent-1"); err == nil {
		t.Error("viewer should NOT be allowed to start, got nil error")
	}

	// Viewer should NOT be able to restart.
	if err := authorizer.Authorize(viewer, "restart", "agent-1"); err == nil {
		t.Error("viewer should NOT be allowed to restart, got nil error")
	}

	// Viewer should NOT be able to destroy.
	if err := authorizer.Authorize(viewer, "destroy", "agent-1"); err == nil {
		t.Error("viewer should NOT be allowed to destroy, got nil error")
	}
}

func TestAuthorize_OperatorCanStopAssignedAgent(t *testing.T) {
	t.Parallel()

	operator := &User{
		ID:     "operator-carol",
		Role:   RoleOperator,
		Agents: []string{"agent-1", "agent-2"},
		Teams:  []string{"team-alpha"},
	}

	authorizer := NewAuthorizer(nil, testLogger())

	// Operator should be able to stop an assigned agent.
	if err := authorizer.Authorize(operator, "stop", "agent-1"); err != nil {
		t.Errorf("operator should be allowed to stop assigned agent, got error: %v", err)
	}

	// Operator should be able to stop agent in assigned team.
	if err := authorizer.Authorize(operator, "stop", "team-alpha"); err != nil {
		t.Errorf("operator should be allowed to stop in assigned team, got error: %v", err)
	}

	// Operator should NOT be able to stop an unassigned agent.
	if err := authorizer.Authorize(operator, "stop", "agent-3"); err == nil {
		t.Error("operator should NOT be allowed to stop unassigned agent, got nil error")
	}

	// Operator should be able to list (even without specific resource).
	if err := authorizer.Authorize(operator, "list", ""); err != nil {
		t.Errorf("operator should be allowed to list, got error: %v", err)
	}

	// Operator should be able to restart an assigned agent.
	if err := authorizer.Authorize(operator, "restart", "agent-2"); err != nil {
		t.Errorf("operator should be allowed to restart assigned agent, got error: %v", err)
	}
}

func TestAuthorize_AdminCanDoAnything(t *testing.T) {
	t.Parallel()

	admin := &User{
		ID:   "admin-dave",
		Role: RoleAdmin,
	}

	authorizer := NewAuthorizer(nil, testLogger())

	actions := []string{"start", "stop", "restart", "destroy", "list", "status", "logs"}
	resources := []string{"", "any-agent", "any-team", "unknown-resource"}

	for _, action := range actions {
		for _, resource := range resources {
			if err := authorizer.Authorize(admin, action, resource); err != nil {
				t.Errorf("admin should be allowed to %s on %q, got error: %v", action, resource, err)
			}
		}
	}
}

func TestAuthorize_NilUser(t *testing.T) {
	t.Parallel()

	authorizer := NewAuthorizer(nil, testLogger())

	if err := authorizer.Authorize(nil, "list", ""); err == nil {
		t.Error("expected error for nil user, got nil")
	}
}

func TestAuthorize_UnknownAction(t *testing.T) {
	t.Parallel()

	user := &User{
		ID:   "test-user",
		Role: RoleAdmin,
	}

	authorizer := NewAuthorizer(nil, testLogger())

	if err := authorizer.Authorize(user, "invalid-action", ""); err == nil {
		t.Error("expected error for unknown action, got nil")
	}
}

func TestAddUser(t *testing.T) {
	t.Parallel()

	authorizer := NewAuthorizer(nil, testLogger())

	user := User{
		ID:        "new-user",
		Role:      RoleOperator,
		TokenHash: HashToken("some-token"),
	}

	if err := authorizer.AddUser(user); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify user was added.
	got := authorizer.GetUser("new-user")
	if got == nil {
		t.Fatal("expected user, got nil")
	}
	if got.Role != RoleOperator {
		t.Errorf("expected role operator, got %q", got.Role)
	}

	// Duplicate should fail.
	if err := authorizer.AddUser(user); err == nil {
		t.Error("expected error for duplicate user, got nil")
	}
}

func TestAddUser_EmptyID(t *testing.T) {
	t.Parallel()

	authorizer := NewAuthorizer(nil, testLogger())

	user := User{Role: RoleViewer}
	if err := authorizer.AddUser(user); err == nil {
		t.Error("expected error for empty user ID, got nil")
	}
}

func TestAddUser_InvalidRole(t *testing.T) {
	t.Parallel()

	authorizer := NewAuthorizer(nil, testLogger())

	user := User{ID: "bad-role", Role: Role("superadmin")}
	if err := authorizer.AddUser(user); err == nil {
		t.Error("expected error for invalid role, got nil")
	}
}

func TestRemoveUser(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "user-1", Role: RoleViewer, TokenHash: HashToken("token-1")},
		{ID: "user-2", Role: RoleOperator, TokenHash: HashToken("token-2")},
	}

	authorizer := NewAuthorizer(users, testLogger())

	if err := authorizer.RemoveUser("user-1"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if got := authorizer.GetUser("user-1"); got != nil {
		t.Error("expected user to be removed, but it still exists")
	}

	// Token for removed user should no longer authenticate.
	if _, err := authorizer.Authenticate("token-1"); err == nil {
		t.Error("expected error authenticating with removed user's token, got nil")
	}

	// Removing non-existent user should fail.
	if err := authorizer.RemoveUser("nonexistent"); err == nil {
		t.Error("expected error removing nonexistent user, got nil")
	}
}

func TestListUsers(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "charlie", Role: RoleViewer},
		{ID: "alice", Role: RoleAdmin},
		{ID: "bob", Role: RoleOperator},
	}

	authorizer := NewAuthorizer(users, testLogger())

	listed := authorizer.ListUsers()
	if len(listed) != 3 {
		t.Fatalf("expected 3 users, got %d", len(listed))
	}

	// Should be sorted alphabetically.
	if listed[0].ID != "alice" {
		t.Errorf("expected first user 'alice', got %q", listed[0].ID)
	}
	if listed[1].ID != "bob" {
		t.Errorf("expected second user 'bob', got %q", listed[1].ID)
	}
	if listed[2].ID != "charlie" {
		t.Errorf("expected third user 'charlie', got %q", listed[2].ID)
	}
}

func TestUpdateUser(t *testing.T) {
	t.Parallel()

	users := []User{
		{ID: "user-1", Role: RoleViewer, Teams: []string{"team-a"}},
	}

	authorizer := NewAuthorizer(users, testLogger())

	newRole := RoleOperator
	newTeams := []string{"team-a", "team-b"}
	newAgents := []string{"agent-x"}

	if err := authorizer.UpdateUser(RoleAdmin, "user-1", &newRole, newTeams, newAgents); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	got := authorizer.GetUser("user-1")
	if got == nil {
		t.Fatal("expected user, got nil")
	}
	if got.Role != RoleOperator {
		t.Errorf("expected role operator, got %q", got.Role)
	}
	if len(got.Teams) != 2 {
		t.Errorf("expected 2 teams, got %d", len(got.Teams))
	}
	if len(got.Agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(got.Agents))
	}
}

func TestUpdateUser_NotFound(t *testing.T) {
	t.Parallel()

	authorizer := NewAuthorizer(nil, testLogger())

	newRole := RoleAdmin
	if err := authorizer.UpdateUser(RoleAdmin, "nonexistent", &newRole, nil, nil); err == nil {
		t.Error("expected error for nonexistent user, got nil")
	}
}

func TestValidateRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role    Role
		wantErr bool
	}{
		{RoleAdmin, false},
		{RoleOperator, false},
		{RoleViewer, false},
		{Role("superadmin"), true},
		{Role(""), true},
	}

	for _, tt := range tests {
		err := ValidateRole(tt.role)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateRole(%q): wantErr=%v, got err=%v", tt.role, tt.wantErr, err)
		}
	}
}

func TestOperatorNoScopeDeniesAccess(t *testing.T) {
	t.Parallel()

	// An operator with no teams or agents specified should be denied access (secure default).
	operator := &User{
		ID:   "operator-no-scope",
		Role: RoleOperator,
	}

	authorizer := NewAuthorizer(nil, testLogger())

	if err := authorizer.Authorize(operator, "stop", "any-agent"); err == nil {
		t.Errorf("operator with empty scope should be denied access, but was allowed")
	}
}

func TestOperatorWithExplicitScopeHasAccess(t *testing.T) {
	t.Parallel()

	// An operator with explicit scope should have access to matching resources.
	operator := &User{
		ID:     "operator-scoped",
		Role:   RoleOperator,
		Agents: []string{"my-agent"},
	}

	authorizer := NewAuthorizer(nil, testLogger())

	if err := authorizer.Authorize(operator, "stop", "my-agent"); err != nil {
		t.Errorf("operator with matching scope should have access, got error: %v", err)
	}
	if err := authorizer.Authorize(operator, "stop", "other-agent"); err == nil {
		t.Errorf("operator should not have access to out-of-scope agent")
	}
}

func TestHashToken(t *testing.T) {
	t.Parallel()

	// Same input should produce same hash.
	hash1 := HashToken("my-secret")
	hash2 := HashToken("my-secret")
	if hash1 != hash2 {
		t.Errorf("same input should produce same hash: %q != %q", hash1, hash2)
	}

	// Different input should produce different hash.
	hash3 := HashToken("different-secret")
	if hash1 == hash3 {
		t.Error("different inputs should produce different hashes")
	}

	// Hash should be 64 hex characters (SHA-256 = 32 bytes = 64 hex chars).
	if len(hash1) != 64 {
		t.Errorf("expected 64-char hex hash, got %d chars", len(hash1))
	}
}
