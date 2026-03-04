package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"sync"
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
	"start":   true,
	"stop":    true,
	"restart": true,
	"destroy": true,
	"list":    true,
	"status":  true,
	"logs":    true,
}

// Authorizer provides authentication and authorization for users.
type Authorizer struct {
	mu     sync.RWMutex
	users  map[string]*User  // keyed by user ID
	tokens map[string]string // token hash -> user ID
	logger *slog.Logger
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
		a.users[u.ID] = &u
		if u.TokenHash != "" {
			a.tokens[u.TokenHash] = u.ID
		}
	}

	return a
}

// Authenticate verifies a raw token and returns the associated user.
// The token is hashed with SHA-256 and compared against stored hashes.
func (a *Authorizer) Authenticate(token string) (*User, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	hash := hashToken(token)
	userID, ok := a.tokens[hash]
	if !ok {
		return nil, fmt.Errorf("invalid token")
	}

	user, ok := a.users[userID]
	if !ok {
		return nil, fmt.Errorf("user %q not found for token", userID)
	}

	cp := *user
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

	// For operator and viewer, check resource scope.
	if resource == "" {
		// No specific resource; allow for list-type actions.
		return nil
	}

	if !a.hasAccess(user, resource) {
		return fmt.Errorf("permission denied: user %q does not have access to resource %q", user.ID, resource)
	}

	return nil
}

// hasAccess checks whether a user has access to a specific resource.
// If the user has no teams or agents configured, access is granted to all resources.
func (a *Authorizer) hasAccess(user *User, resource string) bool {
	// If the user has no scope restrictions, they can access everything.
	if len(user.Teams) == 0 && len(user.Agents) == 0 {
		return true
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

// AddUser adds a new user to the authorizer.
func (a *Authorizer) AddUser(user User) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if user.ID == "" {
		return fmt.Errorf("user ID is required")
	}

	if err := ValidateRole(user.Role); err != nil {
		return err
	}

	if _, exists := a.users[user.ID]; exists {
		return fmt.Errorf("user %q already exists", user.ID)
	}

	u := user
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

	a.logger.Info("user removed", "user_id", userID)
	return nil
}

// UpdateUser updates an existing user's fields. Only non-nil fields are updated.
func (a *Authorizer) UpdateUser(userID string, role *Role, teams []string, agents []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	user, ok := a.users[userID]
	if !ok {
		return fmt.Errorf("user %q not found", userID)
	}

	if role != nil {
		if err := ValidateRole(*role); err != nil {
			return err
		}
		user.Role = *role
	}

	if teams != nil {
		user.Teams = teams
	}

	if agents != nil {
		user.Agents = agents
	}

	a.logger.Info("user updated", "user_id", userID, "role", user.Role)
	return nil
}

// ListUsers returns all users sorted alphabetically by ID.
func (a *Authorizer) ListUsers() []User {
	a.mu.RLock()
	defer a.mu.RUnlock()

	users := make([]User, 0, len(a.users))
	for _, u := range a.users {
		users = append(users, *u)
	}

	sort.Slice(users, func(i, j int) bool {
		return users[i].ID < users[j].ID
	})

	return users
}

// GetUser returns a copy of a specific user by ID, or nil if not found.
func (a *Authorizer) GetUser(userID string) *User {
	a.mu.RLock()
	defer a.mu.RUnlock()

	user, ok := a.users[userID]
	if !ok {
		return nil
	}
	cp := *user
	return &cp
}

// HashToken returns the SHA-256 hex digest of a raw token string.
func HashToken(raw string) string {
	return hashToken(raw)
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
