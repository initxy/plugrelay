// Package manifests embeds the official upstream manifests maintained in the
// repository, one YAML file per upstream, and their JSON Schema.
package manifests

import "embed"

// FS holds every official manifest (*.yaml) and schema.json.
//
//go:embed *.yaml schema.json
var FS embed.FS
