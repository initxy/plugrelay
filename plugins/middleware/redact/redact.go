// Package redact is the optional middleware.redact plugin: before a
// request leaves the relay it masks secrets in the user's text — by
// default API-key and token-like strings, email addresses and phone
// numbers; any other detector of textscan (cards, IBANs, ID numbers,
// private keys) on request — and any extra regular expressions the
// operator configures.
//
// Only text written by the user role is scanned (message text, not tool
// results, system prompts or images). Matches become "[REDACTED:<kind>]".
//
// Like middleware.param-policy it edits on two paths: MutateBody rewrites
// only the text fields listed in Info.Mutates (patch tier), and OnAttempt
// rewrites the IR's user text blocks (convert tier). Redacting already
// redacted text is a no-op, so retries are safe.
//
// It belongs to the content-safety feature, off by default, because
// rewriting user text costs prompt-cache hits whenever a match appears.
package redact

import (
	"fmt"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/textscan"
)

// ID is the plugin id.
const ID = "middleware.redact"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// Defaults are the detectors that run unless "detect" says otherwise.
var Defaults = []string{textscan.Secret, textscan.Email, textscan.Phone}

// Redactor implements plugin.AttemptHook and plugin.BodyMutator.
type Redactor struct{}

// New returns the plugin.
func New() *Redactor { return &Redactor{} }

// Info implements plugin.Plugin.
func (*Redactor) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindMiddleware,
		Version:     "1.0.0",
		Description: "Masks secrets, emails and phone numbers in user text before sending.",
		FailPolicy:  plugin.FailClosed,
		Mutates:     textscan.UserTextFields,
		Settings: []plugin.Setting{
			{Key: "detect", Type: plugin.TypeList, Default: Defaults, Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Override, Enum: textscan.Detectors, EnumLabels: textscan.DetectorLabels,
				Label: plugin.T("Mask before sending", "发出前遮掉"),
				Help: plugin.T("Found in what users write and masked before the request goes upstream.",
					"在用户输入里找到这些内容，发往上游前遮掉。")},
			{Key: "patterns", Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Label: plugin.T("More to mask (patterns)", "其他要遮掉的内容（正则）"),
				Help:  plugin.T("Regular expressions (RE2 syntax).", "正则表达式，RE2 语法。")},
		},
	}
}

func (r *Redactor) rules(c *plugin.Call) ([]textscan.Rule, error) {
	var out []textscan.Rule
	detect := Defaults
	if v := c.Setting("detect"); v.IsSet() {
		detect = v.List()
	}
	for _, d := range detect {
		if rule, ok := textscan.Builtin(d); ok {
			out = append(out, rule)
		}
	}
	for i, p := range c.Setting("patterns").List() {
		rule, err := textscan.Pattern("custom", p)
		if err != nil {
			return nil, fmt.Errorf("redact: pattern %d: %w", i, err)
		}
		out = append(out, rule)
	}
	return out, nil
}

// mask replaces every match of rules in s.
func mask(rules []textscan.Rule, s string) string { return textscan.Mask(rules, s) }

// OnAttempt implements plugin.AttemptHook for the convert tier.
func (r *Redactor) OnAttempt(c *plugin.Call, a *plugin.Attempt) error {
	if a.Tier != plugin.TierConvert {
		return nil
	}
	rules, err := r.rules(c)
	if err != nil || len(rules) == 0 {
		return err
	}
	req, err := c.Request()
	if err != nil {
		return err
	}
	textscan.MaskRequest(req, func(s string) string { return mask(rules, s) })
	return nil
}

// MutateBody implements plugin.BodyMutator for the patch tier.
func (r *Redactor) MutateBody(c *plugin.Call, a *plugin.Attempt, body []byte) ([]byte, error) {
	rules, err := r.rules(c)
	if err != nil || len(rules) == 0 {
		return body, err
	}
	return textscan.MaskBody(body, plugin.BodyProtocol(c, a), func(s string) string { return mask(rules, s) })
}
