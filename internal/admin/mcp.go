package admin

import (
	"errors"
	"sort"

	"github.com/initxy/plugrelay/internal/mcp"
)

// MCP gateway servers. The list lives in plugin KV (mcp.KVNamespace);
// secret header values are sealed there and never returned.

func (h *Handler) mcpRegistry() (*mcp.Registry, error) {
	if h.d.MCP == nil {
		return nil, errNotFound(eMCPOff)
	}
	return h.d.MCP.Registry(), nil
}

func mcpErr(err error) error {
	var fe *mcp.FieldError
	switch {
	case errors.As(err, &fe):
		return fieldInvalid(fe.Field, mcpFieldText(fe))
	case errors.Is(err, mcp.ErrNotFound):
		return errNotFound(eNotFound.with("object", objMCPServer))
	case errors.Is(err, mcp.ErrExists):
		return errConflict(eMCPServerExists)
	}
	return err
}

func mcpFieldText(fe *mcp.FieldError) worded {
	switch fe.Code {
	case "id_format":
		return eIDFormat
	case "immutable":
		return eImmutable
	case "url_no_credentials":
		return eURLNoCreds
	case "secret_reenter":
		return eSecretReenter
	case "transport":
		return oneOf(mcp.TransportAuto, mcp.TransportStreamable, mcp.TransportSSE)
	case "invalid":
		return eInvalidValue
	case "no_master_key":
		return eNoMasterKey
	case "proxy":
		return eURLProxy
	case "timeout_range":
		return eNumberRange.with("min", 0, "max", 3600)
	case "header_invalid":
		return eHeaderInvalid.with("reason", fe.Message)
	}
	return eValueRejected.with("reason", fe.Message)
}

func (h *Handler) listMCPServers(c *call) error {
	reg, err := h.mcpRegistry()
	if err != nil {
		return err
	}
	all, err := reg.List(c.ctx)
	if err != nil {
		return err
	}
	out := make([]mcp.View, 0, len(all))
	for _, s := range all {
		out = append(out, mcp.ViewOf(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return c.json(200, map[string]any{"items": out})
}

func (h *Handler) getMCPServer(c *call) error {
	reg, err := h.mcpRegistry()
	if err != nil {
		return err
	}
	s, err := reg.Get(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return mcpErr(err)
	}
	return c.json(200, mcp.ViewOf(*s))
}

func (h *Handler) createMCPServer(c *call) error {
	reg, err := h.mcpRegistry()
	if err != nil {
		return err
	}
	var in mcp.Input
	if err := c.decode(&in); err != nil {
		return err
	}
	s, err := reg.Create(c.ctx, in)
	if err != nil {
		return mcpErr(err)
	}
	c.audit("mcp.server.create", "mcp_server", s.ID, s.Name, []change{{Field: "url", To: s.URL}})
	return c.json(201, mcp.ViewOf(*s))
}

func (h *Handler) updateMCPServer(c *call) error {
	reg, err := h.mcpRegistry()
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	old, err := reg.Get(c.ctx, id)
	if err != nil {
		return mcpErr(err)
	}
	var in mcp.Input
	if err := c.decode(&in); err != nil {
		return err
	}
	s, err := reg.Update(c.ctx, id, in)
	if err != nil {
		return mcpErr(err)
	}
	var changes []change
	if old.URL != s.URL {
		changes = append(changes, change{Field: "url", From: old.URL, To: s.URL})
	}
	if old.Enabled != s.Enabled {
		changes = append(changes, change{Field: "enabled", From: old.Enabled, To: s.Enabled})
	}
	for k := range in.SecretHeaders {
		changes = append(changes, change{Field: "secret_headers." + k, To: "(changed)"})
	}
	c.audit("mcp.server.update", "mcp_server", s.ID, s.Name, changes)
	return c.json(200, mcp.ViewOf(*s))
}

func (h *Handler) deleteMCPServer(c *call) error {
	reg, err := h.mcpRegistry()
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if err := reg.Delete(c.ctx, id); err != nil {
		return mcpErr(err)
	}
	c.audit("mcp.server.delete", "mcp_server", id, "", nil)
	return c.noContent()
}

func (h *Handler) testMCPServer(c *call) error {
	reg, err := h.mcpRegistry()
	if err != nil {
		return err
	}
	s, err := reg.Get(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return mcpErr(err)
	}
	return c.json(200, testOutcome([]mcp.TestResult{h.d.MCP.Test(c.ctx, s)}))
}
