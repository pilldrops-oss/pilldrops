package store

import "context"

// ConsoleAccounts adapts a Store to the dashboard's account sign-in
// (webui.AdminStore), so webui never imports this package or sees its structs.
// The control plane and a single-box node both use it.
type ConsoleAccounts struct{ st *Store }

// AccountsOf is st as the dashboard's account store.
func AccountsOf(st *Store) ConsoleAccounts { return ConsoleAccounts{st: st} }

func (a ConsoleAccounts) AdminCount(ctx context.Context) (int, error) {
	return a.st.AdminCount(ctx)
}

func (a ConsoleAccounts) CreateFirstAdmin(ctx context.Context, email, passHash string) error {
	return a.st.CreateFirstAdmin(ctx, email, passHash)
}

func (a ConsoleAccounts) AdminByEmail(ctx context.Context, email string) (string, string, string, bool, error) {
	u, found, err := a.st.AdminByEmail(ctx, email)
	if err != nil || !found {
		return "", "", "", false, err
	}
	return u.Email, u.PassHash, u.Role, true, nil
}

func (a ConsoleAccounts) TouchAdminLogin(ctx context.Context, email string) {
	a.st.TouchAdminLogin(ctx, email)
}

func (a ConsoleAccounts) UpdateAdminPassword(ctx context.Context, email, passHash string) error {
	return a.st.UpdateAdminPassword(ctx, email, passHash)
}
