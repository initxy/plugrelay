package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/initxy/plugrelay/internal/store"
)

func modelCommand() *command {
	return &command{
		name:    "model",
		summary: "Manage client-facing model names and the upstreams that serve them.",
		usage:   "model <add|list|rm> [flags]",
		subs: []*command{
			{
				name: "add",
				summary: `Add a model, or replace the routes of an existing one.

Each --route is upstream:upstream_model[:priority[:weight]]. Higher
priority is tried first; weight splits new sessions within a priority
(default 1). The upstream model may contain colons (ollama:qwen3:8b);
an empty one means the client-facing name.`,
				usage:   "model add --name N --route upstream:model[:priority[:weight]]... [--display-name S] [--disabled]",
				example: "plugrelay model add --name deepseek-chat --route deepseek:deepseek-chat:10 --route openrouter:deepseek/deepseek-chat:0",
				run:     runModelAdd,
			},
			{
				name:    "list",
				summary: "List models with their routes.",
				usage:   "model list [--json]",
				example: "plugrelay model list --json",
				run:     runModelList,
			},
			{
				name:    "rm",
				summary: "Delete a model and its routes.",
				usage:   "model rm --name N",
				example: "plugrelay model rm --name deepseek-chat",
				run:     runModelRm,
			},
		},
	}
}

func runModelAdd(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("name", "", "client-facing model name (required)")
	display := fs.String("display-name", "", "name shown in model lists")
	var routeSpecs repeatFlag
	fs.Var(&routeSpecs, "route", "upstream:upstream_model[:priority[:weight]]; repeatable (required)")
	disabled := fs.Bool("disabled", false, "add the model switched off")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("--name is required")
	}
	if len(routeSpecs) == 0 {
		return usagef("at least one --route is required")
	}
	routes := make([]store.Route, 0, len(routeSpecs))
	for _, s := range routeSpecs {
		rt, err := parseRoute(s)
		if err != nil {
			return usagef("%v", err)
		}
		routes = append(routes, rt)
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	for _, rt := range routes {
		if _, err := st.GetUpstream(r.ctx(), rt.UpstreamID); err != nil {
			return notFound("upstream", rt.UpstreamID, err)
		}
	}
	m, err := st.GetModel(r.ctx(), *name)
	verb := "updated"
	switch {
	case errors.Is(err, store.ErrNotFound):
		m, verb = &store.Model{Name: *name}, "added"
		m.Info.ID = *name
	case err != nil:
		return err
	}
	if *display != "" {
		m.Info.DisplayName = *display
	}
	m.Enabled = !*disabled
	m.ManualDisabled = *disabled
	if err := st.PutModel(r.ctx(), m); err != nil {
		return err
	}
	if err := st.ReplaceRoutes(r.ctx(), m.Name, routes); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Model %s %s with %d route(s)\n", m.Name, verb, len(routes))
	return nil
}

type modelRow struct {
	store.Model
	Routes []store.Route `json:"routes"`
}

func runModelList(r *runner, cmd *command, args []string) error {
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
	models, err := st.ListModels(r.ctx())
	if err != nil {
		return err
	}
	routes, err := st.ListRoutes(r.ctx(), "")
	if err != nil {
		return err
	}
	by := map[string][]store.Route{}
	for _, rt := range routes {
		by[rt.Model] = append(by[rt.Model], rt)
	}
	out := make([]modelRow, 0, len(models))
	for _, m := range models {
		rs := by[m.Name]
		if rs == nil {
			rs = []store.Route{}
		}
		out = append(out, modelRow{Model: m, Routes: rs})
	}
	if *asJSON {
		return printJSON(r.env.Stdout, out)
	}
	rows := make([][]string, 0, len(out))
	for _, m := range out {
		var parts []string
		for _, rt := range m.Routes {
			parts = append(parts, fmt.Sprintf("%s:%s (p%d w%d)", rt.UpstreamID, rt.UpstreamModel, rt.Priority, rt.Weight))
		}
		rows = append(rows, []string{m.Name, strconv.FormatBool(m.Enabled), orDash(strings.Join(parts, ", "))})
	}
	return table(r.env.Stdout, []string{"NAME", "ENABLED", "ROUTES"}, rows)
}

func runModelRm(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	name := fs.String("name", "", "model name (required)")
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
	if err := st.DeleteModel(r.ctx(), *name); err != nil {
		return notFound("model", *name, err)
	}
	fmt.Fprintf(r.env.Stdout, "Model %s deleted\n", *name)
	return nil
}
