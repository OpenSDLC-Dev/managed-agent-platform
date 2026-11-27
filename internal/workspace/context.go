package workspace

import "context"

const Default = "default"

type contextKey struct{}

// WithID carries the storage key resolved from a credential or its parent resource.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// ID returns the selected workspace. An unbound context cannot match a resource.
func ID(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// Matches checks the optional public selector against the authenticated binding.
func Matches(values []string, publicID string) bool {
	return len(values) == 0 || len(values) == 1 && values[0] == publicID
}
