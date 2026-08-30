package auth

import (
	"context"

	"github.com/google/uuid"
)

// User is the authenticated identity handlers read from the request context.
// It is the only contract between the auth middleware and the handlers.
type User struct {
	ID        uuid.UUID
	Email     string
	Name      string
	AvatarURL string
	// IsAdmin is set by the middleware: true for the bearer token, and for a
	// session whose email is in ADMIN_EMAILS.
	IsAdmin bool
}

type ctxKey struct{}

// WithUser attaches the authenticated user to the context.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the authenticated user. The middleware guarantees presence
// on every authenticated route; the second return guards programmer error
// (a handler mounted outside the middleware), not a runtime condition.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}
