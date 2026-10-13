package testutil

import "github.com/dorinpuscasu25/recapproject/internal/identity"

func identityInvalidate() { identity.InvalidateAllSessionCache() }
