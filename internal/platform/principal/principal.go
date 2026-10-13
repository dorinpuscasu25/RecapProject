// Package principal carries the authenticated caller through request contexts.
package principal

import (
	"context"

	"github.com/google/uuid"
)

type Kind string

const (
	KindUser    Kind = "user"
	KindService Kind = "service"
	KindSystem  Kind = "system"
)

type Principal struct {
	Kind        Kind
	ID          uuid.UUID // user id or service account id
	SessionID   uuid.UUID // zero for service accounts
	Roles       []string
	Permissions map[string]bool
	IP          string
	IPPrefix    string
	TokenID     string
}

func (p *Principal) Can(perm string) bool { return p != nil && p.Permissions[perm] }

func (p *Principal) HasRole(role string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type ctxKey struct{}

func With(ctx context.Context, p *Principal) context.Context { return context.WithValue(ctx, ctxKey{}, p) }

func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// System returns a principal for background workers.
func System() *Principal { return &Principal{Kind: KindSystem} }
