// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package auth provides RBAC-based user authentication and authorization for the Hive control plane.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/brmurrell3/hive/internal/types"
)

// Role defines the authorization level of a user.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// validRoles is the set of recognized roles.
var validRoles = map[Role]bool{
	RoleAdmin:    true,
	RoleOperator: true,
	RoleViewer:   true,
}

// ValidateRole checks that a role string is one of the recognized roles.
func ValidateRole(r Role) error {
	if !validRoles[r] {
		return fmt.Errorf("invalid role %q: must be admin, operator, or viewer", r)
	}
	return nil
}

// User represents a user in the RBAC system.
type User struct {
	ID        string   `json:"id" yaml:"id"`
	Name      string   `json:"name,omitempty" yaml:"name,omitempty"`
	Role      Role     `json:"role" yaml:"role"`
	TokenHash string   `json:"token_hash" yaml:"token_hash"`
	Teams     []string `json:"teams,omitempty" yaml:"teams,omitempty"`
	Agents    []string `json:"agents,omitempty" yaml:"agents,omitempty"`
}

// viewerActions are the actions a viewer is allowed to perform.
var viewerActions = map[string]bool{
	"list":   true,
	"status": true,
	"logs":   true,
}

// allActions are all recognized actions in the system.
var allActions = map[string]bool{
	"start":     true,
	"stop":      true,
	"restart":   true,
	"destroy":   true,
	"list":      true,
	"status":    true,
	"logs":      true,
	"chat":      true,
	"invoke":    true,
	"broadcast": true,
	"connect":   true,
	"exec":      true,
	"scale":     true,
	"manage":    true,
	"view":      true,
	"run":       true,
}

// Authorizer provides authentication and authorization for users.
type Authorizer struct {
	mu             sync.RWMutex
	users          map[string]*User  // keyed by user ID
	tokens         map[string]string // token hash -> user ID
	logger         *slog.Logger
	emptyScopeWarn sync.Map
}

// NewAuthorizer creates a new Authorizer populated with the given users.
func NewAuthorizer(users []User, logger *slog.Logger) *Authorizer {
	a := &Authorizer{
		users:  make(map[string]*User),
		tokens: make(map[string]string),
		logger: logger,
	}

	for i := range users {
		u := users[i]

		// Reject empty user IDs.
		if u.ID == "" {
			logger.Warn("skipping user with empty ID", "index", i)
			continue
		}

		// Validate user ID format for safe use in NATS subjects.
		if err := types.ValidateSubjectComponent("user_id", u.ID); err != nil {
			logger.Warn("skipping user with invalid ID format", "user_id", u.ID, "error", err, "index", i)
			continue
		}

		// Validate role.
		if !validRoles[u.Role] {
			logger.Warn("skipping user with invalid role", "user_id", u.ID, "role", u.Role)
			continue
		}

		// Check for duplicate user IDs — remove stale token hash before overwriting.
		if existing, dup := a.users[u.ID]; dup {
			logger.Warn("duplicate user ID, overwriting previous entry", "user_id", u.ID)
			if existing.TokenHash != "" {
				delete(a.tokens, existing.TokenHash)
			}
		}

		a.users[u.ID] = &u
		if u.TokenHash != "" {
			a.tokens[u.TokenHash] = u.ID
		}
	}

	// Startup-time check: log at ERROR level for non-admin users with empty scope.
	// Empty scope denies access; the user must have Teams or Agents set.
	for _, u := range a.users {
		if u.Role != RoleAdmin && len(u.Teams) == 0 && len(u.Agents) == 0 {
			logger.Error("non-admin user has empty scope and will be denied access to all resources; "+
				"set Teams or Agents to grant access",
				"user_id", u.ID,
				"role", u.Role,
			)
			a.emptyScopeWarn.Store(u.ID, true)
		}
	}

	return a
}

// Authenticate verifies a raw token and returns the associated user.
// The token is hashed with SHA-256 and compared against stored hashes using
// constant-time comparison to prevent timing side-channel attacks.
// Failed authentication attempts are logged at WARN level for audit purposes
// (the token itself is never logged).
//
// Note: Callers must provide rate limiting upstream. See internal/production.RateLimiter.
func (a *Authorizer) Authenticate(token string) (*User, error) {
	if token == "" {
		return nil, fmt.Errorf("invalid credentials")
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	hash := hashToken(token)

	// Use constant-time comparison to prevent timing side-channel attacks.
	// We scan ALL entries rather than using map lookup to avoid leaking
	// whether a hash prefix exists via timing. We do NOT break on match
	// so that the loop always iterates all entries regardless of where
	// the match is, preventing timing side-channels based on map position.
	var userID string
	found := false
	for h, uid := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(h), []byte(hash)) == 1 {
			userID = uid
			found = true
			// NO break - continue iterating all entries for constant time
		}
	}
	if !found {
		a.logger.Warn("authentication failed: unknown token hash")
		return nil, fmt.Errorf("invalid credentials")
	}

	user, ok := a.users[userID]
	if !ok {
		a.logger.Warn("authentication failed: token maps to missing user",
			"user_id", userID,
		)
		return nil, fmt.Errorf("invalid credentials")
	}

	cp := *user
	if user.Teams != nil {
		cp.Teams = make([]string, len(user.Teams))
		copy(cp.Teams, user.Teams)
	}
	if user.Agents != nil {
		cp.Agents = make([]string, len(user.Agents))
		copy(cp.Agents, user.Agents)
	}
	return &cp, nil
}

// Authorize checks whether a user is permitted to perform the given action
// on the given resource. Resources are either agent IDs or team IDs.
//
// Authorization rules:
//   - Admin: all actions on all resources
//   - Operator: all actions on assigned teams/agents
//   - Viewer: "list", "status", "logs" only on assigned teams/agents
func (a *Authorizer) Authorize(user *User, action string, resource string) error {
	if user == nil {
		return fmt.Errorf("nil user")
	}

	if !allActions[action] {
		return fmt.Errorf("unknown action %q", action)
	}

	// Admin can do anything.
	if user.Role == RoleAdmin {
		return nil
	}

	// Viewer can only perform read-only actions.
	if user.Role == RoleViewer && !viewerActions[action] {
		return fmt.Errorf("permission denied: viewer cannot perform %q", action)
	}

	// Reject unrecognized roles that slipped past authentication.
	if user.Role != RoleOperator && user.Role != RoleViewer {
		return fmt.Errorf("unrecognized role %q", user.Role)
	}

	// For operator and viewer, check resource scope.
	if resource == "" {
		// No specific resource; allow for list-type actions.
		a.logger.Debug("authorize called with empty resource for non-admin user",
			"user_id", user.ID,
			"role", user.Role,
			"action", action,
		)
		return nil
	}

	if !a.hasAccess(user, resource) {
		return fmt.Errorf("permission denied: user %q does not have access to resource %q", user.ID, resource)
	}

	return nil
}

// hasAccess checks whether a user has access to a specific resource.
//
// SECURITY: Empty scope DENIES access for non-admin users. Users must
// explicitly have Teams and/or Agents set to access specific resources.
// This follows the principle of least privilege: a newly created
// operator/viewer without scope assignments cannot access any resources
// until an admin explicitly grants access.
func (a *Authorizer) hasAccess(user *User, resource string) bool {
	// Empty Teams and Agents means NO access for non-admin users.
	// Admins are handled before hasAccess is called, so this only
	// affects operators and viewers.
	if len(user.Teams) == 0 && len(user.Agents) == 0 {
		if user.Role != RoleAdmin {
			if _, warned := a.emptyScopeWarn.Load(user.ID); !warned {
				a.logger.Error("non-admin user has empty scope, denying access; "+
					"set Teams or Agents on the user record to grant access",
					"user_id", user.ID,
					"role", user.Role,
				)
				a.emptyScopeWarn.Store(user.ID, true)
			}
		}
		return false
	}

	for _, t := range user.Teams {
		if t == resource {
			return true
		}
	}

	for _, ag := range user.Agents {
		if ag == resource {
			return true
		}
	}

	return false
}

// maxUsers is the upper bound on the number of users to prevent unbounded growth.
const maxUsers = 10000

// AddUser adds a new user to the authorizer.
func (a *Authorizer) AddUser(user User) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if user.ID == "" {
		return fmt.Errorf("user ID is required")
	}

	if err := types.ValidateSubjectComponent("user_id", user.ID); err != nil {
		return fmt.Errorf("invalid user ID: %w", err)
	}

	if err := ValidateRole(user.Role); err != nil {
		return err
	}

	if len(a.users) >= maxUsers {
		return fmt.Errorf("maximum number of users (%d) reached", maxUsers)
	}

	if _, exists := a.users[user.ID]; exists {
		return fmt.Errorf("user %q already exists", user.ID)
	}

	for _, t := range user.Teams {
		if err := types.ValidateSubjectComponent("team", t); err != nil {
			return fmt.Errorf("invalid team entry: %w", err)
		}
	}
	for _, ag := range user.Agents {
		if err := types.ValidateSubjectComponent("agent", ag); err != nil {
			return fmt.Errorf("invalid agent entry: %w", err)
		}
	}

	u := user

	// Check for duplicate token hash to prevent two users from sharing
	// the same token (which would be a collision or misconfiguration).
	if u.TokenHash != "" {
		if existingUserID, collision := a.tokens[u.TokenHash]; collision {
			return fmt.Errorf("token hash collision: hash already assigned to user %q", existingUserID)
		}
	}

	a.users[u.ID] = &u
	if u.TokenHash != "" {
		a.tokens[u.TokenHash] = u.ID
	}

	a.logger.Info("user added", "user_id", u.ID, "role", u.Role)
	return nil
}

// RemoveUser removes a user from the authorizer.
func (a *Authorizer) RemoveUser(userID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	user, ok := a.users[userID]
	if !ok {
		return fmt.Errorf("user %q not found", userID)
	}

	if user.TokenHash != "" {
		delete(a.tokens, user.TokenHash)
	}
	delete(a.users, userID)
	a.emptyScopeWarn.Delete(userID)

	a.logger.Info("user removed", "user_id", userID)
	return nil
}

// UpdateUser updates an existing user's fields. Only non-nil fields are updated.
// The callerRole parameter specifies the role of the user performing the update;
// only admin users are permitted to modify user records.
func (a *Authorizer) UpdateUser(callerRole Role, userID string, role *Role, teams []string, agents []string) error {
	if callerRole != RoleAdmin {
		return fmt.Errorf("permission denied: only admin users can update user records")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	user, ok := a.users[userID]
	if !ok {
		return fmt.Errorf("user %q not found", userID)
	}

	oldRole := user.Role

	if role != nil {
		if err := ValidateRole(*role); err != nil {
			return err
		}
		user.Role = *role
	}

	if teams != nil {
		for _, t := range teams {
			if err := types.ValidateSubjectComponent("team", t); err != nil {
				return fmt.Errorf("invalid team entry: %w", err)
			}
		}
		user.Teams = make([]string, len(teams))
		copy(user.Teams, teams)
	}

	if agents != nil {
		for _, ag := range agents {
			if err := types.ValidateSubjectComponent("agent", ag); err != nil {
				return fmt.Errorf("invalid agent entry: %w", err)
			}
		}
		user.Agents = make([]string, len(agents))
		copy(user.Agents, agents)
	}

	a.logger.Info("user updated", "user_id", userID, "old_role", oldRole, "new_role", user.Role)
	return nil
}

// ListUsers returns all users sorted alphabetically by ID.
// Returns deep copies so callers cannot mutate shared slices.
func (a *Authorizer) ListUsers() []User {
	a.mu.RLock()
	defer a.mu.RUnlock()

	users := make([]User, 0, len(a.users))
	for _, u := range a.users {
		cp := *u
		cp.Teams = make([]string, len(u.Teams))
		copy(cp.Teams, u.Teams)
		cp.Agents = make([]string, len(u.Agents))
		copy(cp.Agents, u.Agents)
		users = append(users, cp)
	}

	sort.Slice(users, func(i, j int) bool {
		return users[i].ID < users[j].ID
	})

	return users
}

// GetUser returns a deep copy of a specific user by ID, or nil if not found.
// The returned copy has independent Teams and Agents slices.
func (a *Authorizer) GetUser(userID string) *User {
	a.mu.RLock()
	defer a.mu.RUnlock()

	user, ok := a.users[userID]
	if !ok {
		return nil
	}
	cp := *user
	cp.Teams = make([]string, len(user.Teams))
	copy(cp.Teams, user.Teams)
	cp.Agents = make([]string, len(user.Agents))
	copy(cp.Agents, user.Agents)
	return &cp
}

// HashToken returns the SHA-256 hex digest of a raw token string.
func HashToken(raw string) string {
	return hashToken(raw)
}

// hashToken uses unsalted SHA-256, which is appropriate for API tokens.
// Unlike user passwords, API tokens are high-entropy random strings (typically
// 32+ bytes of cryptographic randomness), making rainbow table and brute-force
// attacks impractical. Salting would add complexity without meaningful security
// benefit in this context. If this were used for user-chosen passwords, a
// proper password hashing function (bcrypt, scrypt, argon2) with salt would
// be required instead.
func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
