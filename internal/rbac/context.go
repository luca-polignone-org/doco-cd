package rbac

import "context"

type principalKey struct{}

// WithPrincipal carries the principal in the context of a run, so that the
// pre-deploy step can check targets/sync once it knows <context>/<project>.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by WithPrincipal.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
