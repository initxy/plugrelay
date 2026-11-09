package plugin

import "context"

// Notice is something an operator may want to be told about, raised by a
// plugin (a budget reaching a threshold) or the core. The notifications
// feature decides whether, when and where it is sent, and drops repeats.
type Notice struct {
	// Event names what happened, e.g. "budget_reached".
	Event string
	// Object is what it happened to.
	Object NoticeObject
	// Detail carries event-specific values (percent, window, reason…).
	Detail map[string]string
	// Key tells repeats apart beyond event and object (a budget's window
	// and period); notices with the same event, object and key are one.
	Key string
}

// NoticeObject names the subject of a notice: Type is "key", "user",
// "group", "credential", "upstream" or "extension".
type NoticeObject struct {
	Type, ID string
}

// NoticeHost is implemented by a Host that forwards notices to the
// notifications feature. Notify never blocks on delivery and drops the
// notice while the feature is off, so it may be called on the request
// path.
type NoticeHost interface {
	Notify(ctx context.Context, n Notice)
}
