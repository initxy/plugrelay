package pipeline

import "github.com/initxy/plugrelay/pkg/plugin"

// errText is err's message for request logs, with any upstream URL query
// cut off (see plugin.RedactURLError).
func errText(err error) string { return plugin.RedactURLError(err).Error() }
