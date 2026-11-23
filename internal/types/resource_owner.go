package types

import "context"

type knowledgeResourceOwnerKey struct{}

// WithKnowledgeResourceOwner marks trusted document-parser writes so resources
// are registered with their owner before any public handle can be returned.
func WithKnowledgeResourceOwner(ctx context.Context, knowledgeID string) context.Context {
	return context.WithValue(ctx, knowledgeResourceOwnerKey{}, knowledgeID)
}

func KnowledgeResourceOwner(ctx context.Context) string {
	id, _ := ctx.Value(knowledgeResourceOwnerKey{}).(string)
	return id
}
