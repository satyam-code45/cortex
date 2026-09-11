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
	// Machine is true when the principal is the static operator token rather
	// than a signed-in person.
	//
	// It exists because one guarantee in this system is specifically about a
	// HUMAN: a write executes only after somebody read the payload and approved
	// it. The bearer token is a shared operator credential that scripts carry —
	// it appears in shell history and CI config — so admitting it on a decision
	// endpoint would let a token holder authorize mail from the owner's mailbox
	// with no human ever having looked. Admin rights are not the question here;
	// being a person is.
	Machine bool
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
