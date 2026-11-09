package cli

import (
	"bufio"
	"errors"
	"fmt"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/store"
)

func userCommand() *command {
	return &command{
		name:    "user",
		summary: "Console account recovery.",
		usage:   "user <reset-password|reset-2fa> [flags]",
		subs: []*command{{
			name: "reset-password",
			summary: `Set a console user's password and turn password sign-in back on.

This is the way back in when single sign-on breaks: it works on the
database directly, so the relay may be stopped. The password comes from
--password, $PLUGRELAY_ADMIN_PASSWORD or stdin. Every session of the user
ends.`,
			usage:   "user reset-password --user EMAIL [--password P]",
			example: "PLUGRELAY_ADMIN_PASSWORD='new long password' plugrelay user reset-password --user admin",
			run:     runUserResetPassword,
		}, {
			name: "reset-2fa",
			summary: `Remove a console user's two-step sign-in (authenticator app, recovery
codes, passkeys).

For a user who lost every second factor. It works on the database
directly, so the relay may be stopped; the user signs in with the first
step alone and can enroll again.`,
			usage:   "user reset-2fa --user EMAIL",
			example: "plugrelay user reset-2fa --user admin",
			run:     runUserResetMFA,
		}},
	}
}

func runUserResetMFA(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("user", "", "the user's username (email) (required)")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--user is required")
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.UserByEmail(r.ctx(), *name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q", *name)
	} else if err != nil {
		return err
	}
	if err := admin.ResetMFA(r.ctx(), st, u.ID); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Two-step sign-in of %s removed\n", u.Email)
	if err := admin.AuditMFAReset(r.ctx(), st, u); err != nil {
		return fmt.Errorf("two-step sign-in removed, but the audit entry failed: %w", err)
	}
	return nil
}

func runUserResetPassword(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("user", "", "the user's username (email) (required)")
	pass := fs.String("password", "", "new password (visible in shell history; prefer $PLUGRELAY_ADMIN_PASSWORD)")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--user is required")
	}
	pw, err := r.adminPassword(bufio.NewReader(r.env.Stdin), *pass, r.env.Interactive())
	if err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	u, err := st.UserByEmail(r.ctx(), *name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q", *name)
	} else if err != nil {
		return err
	}
	if err := st.SetPassword(r.ctx(), u.ID, pw); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Password of %s changed; their sessions ended\n", u.Email)
	changed, err := admin.EnablePasswordLogin(r.ctx(), st)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintln(r.env.Stdout, "Password sign-in was off; it is on again")
	}
	if err := admin.AuditPasswordReset(r.ctx(), st, u, changed); err != nil {
		return fmt.Errorf("password changed, but the audit entry failed: %w", err)
	}
	if u.Disabled {
		fmt.Fprintln(r.env.Stdout, "Note: this user is disabled; enable it in the console or with the admin token")
	}
	return nil
}
