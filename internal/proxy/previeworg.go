package proxy

import (
	"context"
	"net/http"
)

type previewOrgKey struct{}

// WithPreviewOrg records that the request cleared the preview gate for org, the
// sandbox owner read from the store. A context value, not a header, so the
// client cannot set it and an ungated mount carries an empty org, which is refused.
func WithPreviewOrg(ctx context.Context, org string) context.Context {
	return context.WithValue(ctx, previewOrgKey{}, org)
}

// previewOrgFrom returns the authorized org, or "" when no gate ran. Never
// substitutes a default; every default here is a wildcard.
func previewOrgFrom(ctx context.Context) string {
	org, _ := ctx.Value(previewOrgKey{}).(string)
	return org
}

func requestWithPreviewOrg(req *http.Request, org string) *http.Request {
	return req.WithContext(WithPreviewOrg(req.Context(), org))
}
