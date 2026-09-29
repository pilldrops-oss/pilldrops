package orchestrator

import (
	"context"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// adminStoreAdapter adapts *store.Store to webui.AdminStore so webui never
// imports the store package or sees its structs.
type adminStoreAdapter struct{ st *store.Store }

func (a adminStoreAdapter) AdminCount(ctx context.Context) (int, error) {
	return a.st.AdminCount(ctx)
}

func (a adminStoreAdapter) CreateFirstAdmin(ctx context.Context, email, passHash string) error {
	return a.st.CreateFirstAdmin(ctx, email, passHash)
}

func (a adminStoreAdapter) AdminByEmail(ctx context.Context, email string) (string, string, string, bool, error) {
	u, found, err := a.st.AdminByEmail(ctx, email)
	if err != nil || !found {
		return "", "", "", false, err
	}
	return u.Email, u.PassHash, u.Role, true, nil
}

func (a adminStoreAdapter) TouchAdminLogin(ctx context.Context, email string) {
	a.st.TouchAdminLogin(ctx, email)
}

func (a adminStoreAdapter) UpdateAdminPassword(ctx context.Context, email, passHash string) error {
	return a.st.UpdateAdminPassword(ctx, email, passHash)
}
