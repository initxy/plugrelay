package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func keyCommand() *command {
	return &command{
		name:    "key",
		summary: "Manage platform API keys (pr_...) that clients use to call the relay.",
		usage:   "key <create|list|disable|enable|rm> [flags]",
		subs: []*command{
			{
				name:    "create",
				summary: "Create a key and print it. The full key is shown only once.",
				usage:   "key create --name N [--user U] [--group G] [--models a,b] [--expires 30d] [--json]",
				example: "plugrelay key create --name laptop --models claude-sonnet-4-5,gpt-5.5 --expires 90d",
				run:     runKeyCreate,
			},
			{
				name:    "list",
				summary: "List keys (never their secret part).",
				usage:   "key list [--user U] [--group G] [--json]",
				example: "plugrelay key list --json",
				run:     runKeyList,
			},
			keyIDCommand("disable", "Disable a key; clients using it get 401.", func(k *store.Key) { k.Disabled = true }),
			keyIDCommand("enable", "Enable a disabled key.", func(k *store.Key) { k.Disabled = false }),
			{
				name:    "rm",
				summary: "Delete a key permanently.",
				usage:   "key rm --id ID",
				example: "plugrelay key rm --id 3f9c...",
				run:     runKeyRm,
			},
		},
	}
}

func runKeyCreate(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("name", "", "key name (required)")
	user := fs.String("user", "", "owning user id")
	group := fs.String("group", "", "group id whose settings apply")
	var models listFlag
	fs.Var(&models, "models", "comma-separated model allowlist (default: every model)")
	expires := fs.String("expires", "", "lifetime such as 30d, 2w, 12h, or a date YYYY-MM-DD (default: never)")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--name is required")
	}
	exp, err := parseExpires(*expires, r.env.Now())
	if err != nil {
		return usagef("--expires: %v", err)
	}
	k := &store.Key{Name: *name, UserID: *user, GroupID: *group, ExpiresAt: exp}
	if len(models) > 0 {
		raw, _ := json.Marshal([]string(models))
		k.Settings = map[string]json.RawMessage{"models": raw}
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	plain, err := st.CreateKey(r.ctx(), k)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r.env.Stdout, map[string]any{"id": k.ID, "name": k.Name, "key": plain, "expires_at": k.ExpiresAt})
	}
	fmt.Fprintf(r.env.Stdout, "Created key %s (%s)\n\n  %s\n\nThis is the only time the full key is shown. Store it now.\n", k.ID, k.Name, plain)
	return nil
}

func runKeyList(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	user := fs.String("user", "", "only keys of this user")
	group := fs.String("group", "", "only keys of this group")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	keys, err := st.ListKeys(r.ctx(), store.KeyFilter{UserID: *user, GroupID: *group})
	if err != nil {
		return err
	}
	if *asJSON {
		if keys == nil {
			keys = []store.Key{}
		}
		return printJSON(r.env.Stdout, keys)
	}
	now := r.env.Now()
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		status := "active"
		switch {
		case k.Disabled:
			status = "disabled"
		case !k.Usable(now):
			status = "expired"
		}
		rows = append(rows, []string{k.ID, k.Name, "..." + k.Last4, orDash(k.UserID), orDash(k.GroupID),
			keyModels(k), status, fmtTime(k.ExpiresAt), fmtTime(k.LastUsedAt)})
	}
	return table(r.env.Stdout, []string{"ID", "NAME", "KEY", "USER", "GROUP", "MODELS", "STATUS", "EXPIRES", "LAST USED"}, rows)
}

func keyModels(k store.Key) string {
	var ms []string
	if raw, ok := k.Settings["models"]; ok && json.Unmarshal(raw, &ms) == nil && len(ms) > 0 {
		return strings.Join(ms, ",")
	}
	return "*"
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func keyIDCommand(name, summary string, mutate func(*store.Key)) *command {
	return &command{
		name:    name,
		summary: summary,
		usage:   "key " + name + " --id ID",
		example: "plugrelay key " + name + " --id 3f9c...",
		run: func(r *runner, cmd *command, args []string) error {
			id, err := parseID(r, cmd, args)
			if err != nil {
				return err
			}
			st, _, err := r.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			k, err := st.GetKey(r.ctx(), id)
			if err != nil {
				return notFound("key", id, err)
			}
			mutate(k)
			if err := st.UpdateKey(r.ctx(), k); err != nil {
				return err
			}
			fmt.Fprintf(r.env.Stdout, "Key %s (%s) %sd\n", k.ID, k.Name, name)
			return nil
		},
	}
}

func runKeyRm(r *runner, cmd *command, args []string) error {
	id, err := parseID(r, cmd, args)
	if err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeleteKey(r.ctx(), id); err != nil {
		return notFound("key", id, err)
	}
	fmt.Fprintf(r.env.Stdout, "Key %s deleted\n", id)
	return nil
}

// parseID parses a command whose only input is --id.
func parseID(r *runner, cmd *command, args []string) (string, error) {
	fs := r.flags(cmd)
	id := fs.String("id", "", "id (required)")
	pos, err := r.parse(fs, args)
	if err != nil {
		return "", err
	}
	if *id == "" && len(pos) == 1 {
		*id = pos[0]
	} else if len(pos) > 0 {
		return "", usagef("unexpected arguments %q", pos)
	}
	if *id == "" {
		return "", usagef("--id is required")
	}
	return *id, nil
}

func notFound(kind, id string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no %s with id %q", kind, id)
	}
	return err
}
