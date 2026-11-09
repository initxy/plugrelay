package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/store"
)

func presetCommand() *command {
	return &command{
		name:    "preset",
		summary: "Manage presets: one client-facing name served by one of several models.",
		usage:   "preset <add|list|rm> [flags]",
		subs: []*command{
			{
				name: "add",
				summary: `Add a preset, or replace an existing one.

Each --model is a model name, in preference order. --strategy picks among
them: in_order (the first with a usable route, default), cheapest or
fastest. Clients see the preset in their model list; the relay only
serves one of its models when a client asks for the preset by name.`,
				usage:   "preset add --name N --model M... [--strategy in_order|cheapest|fastest]",
				example: "plugrelay preset add --name auto-coding --model claude-sonnet-4-5 --model gpt-5.5 --strategy fastest",
				run:     runPresetAdd,
			},
			{
				name:    "list",
				summary: "List presets.",
				usage:   "preset list [--json]",
				example: "plugrelay preset list --json",
				run:     runPresetList,
			},
			{
				name:    "rm",
				summary: "Delete a preset.",
				usage:   "preset rm --name N",
				example: "plugrelay preset rm --name auto-coding",
				run:     runPresetRm,
			},
		},
	}
}

func runPresetAdd(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("name", "", "client-facing preset name (required)")
	var models repeatFlag
	fs.Var(&models, "model", "candidate model, in preference order; repeatable (required)")
	strategy := fs.String("strategy", string(store.PresetInOrder), "in_order, cheapest or fastest")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--name is required")
	}
	if len(models) == 0 {
		return usagef("at least one --model is required")
	}
	if !slices.Contains(store.PresetStrategies, store.PresetStrategy(*strategy)) {
		return usagef("--strategy must be in_order, cheapest or fastest")
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	for _, m := range models {
		if _, err := st.GetModel(r.ctx(), m); err != nil {
			return notFound("model", m, err)
		}
	}
	verb := "updated"
	if _, err := st.GetPreset(r.ctx(), *name); errors.Is(err, store.ErrNotFound) {
		verb = "added"
	}
	p := &store.Preset{Name: *name, Models: models, Strategy: store.PresetStrategy(*strategy)}
	if err := st.PutPreset(r.ctx(), p); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("a model is named %s; a preset needs a name of its own", *name)
		}
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Preset %s %s with %d model(s)\n", p.Name, verb, len(p.Models))
	return nil
}

func runPresetList(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	ps, err := st.ListPresets(r.ctx())
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r.env.Stdout, nonNilSlice(ps))
	}
	rows := make([][]string, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, []string{p.Name, string(p.Strategy), strings.Join(p.Models, ", ")})
	}
	return table(r.env.Stdout, []string{"NAME", "STRATEGY", "MODELS"}, rows)
}

func runPresetRm(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("name", "", "preset name (required)")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--name is required")
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeletePreset(r.ctx(), *name); err != nil {
		return notFound("preset", *name, err)
	}
	fmt.Fprintf(r.env.Stdout, "Preset %s deleted\n", *name)
	return nil
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
