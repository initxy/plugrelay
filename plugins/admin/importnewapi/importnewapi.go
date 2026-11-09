// Package importnewapi is the admin.import-newapi plugin (sell kit): it
// imports channels, tokens, users and groups from a new-api / one-api
// database or a JSON export, always showing a dry-run report first.
//
// The reading and mapping is done by the core (internal/importnewapi),
// which owns the database drivers; this plugin is the API the console's
// import page calls. Only admins may call it, since it connects to the DSN
// it is given.
package importnewapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.import-newapi"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

// Source is where to read from: a database (driver mysql, postgres or
// sqlite, and its DSN) or Data, a JSON export.
type Source struct {
	Driver string          `json:"driver,omitempty"`
	DSN    string          `json:"dsn,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	// ConfirmLocalFile must be set for a SQLite source: the DSN is a file
	// on the relay's own host, opened read-only.
	ConfirmLocalFile bool `json:"confirm_local_file,omitempty"`
	// Rate converts new-api's USD-based quota into the site currency
	// (site currency units per USD); 0 = 1.
	Rate float64 `json:"rate,omitempty"`
}

// Item is one row of the report.
type Item struct {
	Kind   string `json:"kind"`             // group, upstream, credential, model, user, key, balance
	Source string `json:"source,omitempty"` // new-api id
	ID     string `json:"id"`               // plugrelay id
	Op     string `json:"op"`               // create, update, unchanged, skip
	Reason string `json:"reason,omitempty"`
}

// Report is what an import did, or would do on a dry run.
type Report struct {
	DryRun   bool           `json:"dry_run"`
	Counts   map[string]int `json:"counts"`
	Items    []Item         `json:"items"`
	Warnings []string       `json:"warnings"`
}

// Importer runs imports; the core implements it.
type Importer interface {
	ImportNewAPI(ctx context.Context, src Source, dryRun bool) (*Report, error)
}

// ImporterHost is implemented by a Host offering the importer.
type ImporterHost interface {
	NewAPIImporter() Importer
}

// Plugin implements plugin.Admin.
type Plugin struct{ imp Importer }

var _ plugin.Admin = (*Plugin)(nil)

// New returns the plugin; a nil importer is taken from the Host in Init.
func New(imp Importer) *Plugin { return &Plugin{imp: imp} }

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0",
		Description: "Import channels, tokens, users and groups from new-api / one-api (dry run first)."}
}

// Init implements plugin.Initializer.
func (p *Plugin) Init(_ context.Context, h plugin.Host) error {
	if p.imp == nil {
		ih, ok := h.(ImporterHost)
		if !ok || ih.NewAPIImporter() == nil {
			return errors.New("import-newapi: this relay offers no importer")
		}
		p.imp = ih.NewAPIImporter()
	}
	return nil
}

// Routes implements plugin.Admin.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{{Method: http.MethodPost, Path: "/run", Handler: p.postRun, Role: "admin"}}
}

func (p *Plugin) postRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var in struct {
		Source
		DryRun *bool `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if (in.DSN == "") == (len(in.Data) == 0) {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "give either driver and dsn, or data")
		return
	}
	dry := in.DryRun == nil || *in.DryRun
	rep, err := p.imp.ImportNewAPI(r.Context(), in.Source, dry)
	if err != nil {
		ledger.WriteError(w, http.StatusUnprocessableEntity, "import_failed", err.Error())
		return
	}
	ledger.WriteJSON(w, http.StatusOK, rep)
}
