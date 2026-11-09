package admin

import (
	"context"
	"testing"

	"github.com/initxy/plugrelay/internal/store"
)

// link_by_email never links to an account whose address nobody verified
// (self-service sign-up, import): whoever registered it first could
// otherwise take over the IdP user's login.
func TestSSONoLinkToUnverifiedEmail(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			e, idp, admin := ssoEnv(t, open)
			ctx := context.Background()
			squatter := &store.User{Name: "Eve", Email: "dana@example.com", Role: store.RoleUser, EmailUnverified: true}
			if err := e.st.CreateUser(ctx, squatter, "squatter-password-1"); err != nil {
				t.Fatal(err)
			}
			if u, err := e.st.GetUser(ctx, squatter.ID); err != nil || !u.EmailUnverified {
				t.Fatalf("flag not stored: %+v %v", u, err)
			}
			admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(map[string]any{"link_by_email": true}))
			if _, got := e.ssoTry(idp, "corp", person("d1", "dana@example.com", true)); got != "/login?sso_error=account_exists" {
				t.Fatalf("linked to an unverified account: %q", got)
			}
			if ids, _ := e.st.ListIdentities(ctx, squatter.ID); len(ids) != 0 {
				t.Fatalf("identity linked: %v", ids)
			}
			// A verified (admin-created) account still links.
			dana := &store.User{Name: "Dana", Email: "dana2@example.com", Role: store.RoleMember}
			if err := e.st.CreateUser(ctx, dana, "dana-password-12"); err != nil {
				t.Fatal(err)
			}
			b, got := e.ssoTry(idp, "corp", person("d2", "dana2@example.com", true))
			if got != "/" || userID(b) != dana.ID {
				t.Fatalf("verified account: %q", got)
			}
		})
	}
}
