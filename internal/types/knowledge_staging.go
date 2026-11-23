package types

import "context"

type stagedKnowledgeReadKey struct{}

// WithStagedKnowledgeRead scopes internal cleanup to exact candidate IDs.
// It is never populated from HTTP headers, request metadata or agent input.
func WithStagedKnowledgeRead(ctx context.Context, ids ...string) context.Context {
	return context.WithValue(ctx, stagedKnowledgeReadKey{}, append([]string(nil), ids...))
}

func StagedKnowledgeReadIDs(ctx context.Context) []string {
	ids, _ := ctx.Value(stagedKnowledgeReadKey{}).([]string)
	return ids
}
