package modelrequest

import "context"

type maximumBytesKey struct{}

// WithMaximumBytes records the delivery's complete provider request limit.
func WithMaximumBytes(ctx context.Context, maximum int) context.Context {
	return context.WithValue(ctx, maximumBytesKey{}, maximum)
}

// MaximumBytes uses the delivery limit when the process has an older setting.
func MaximumBytes(ctx context.Context, configured int) int {
	if maximum, present := ctx.Value(maximumBytesKey{}).(int); present {
		return maximum
	}
	return configured
}
