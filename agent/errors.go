package agent

import "errors"

// Sentinel errors distinguish the public failure classes. Wrap them with
// [Error] so callers can use errors.Is and still read the operation.
var (
	ErrCheckpointLimit    = errors.New("checkpoint storage limit exceeded")
	ErrInvalidConfig      = errors.New("invalid config")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrSandboxUnavailable = errors.New("sandbox unavailable")
	ErrRunCancelled       = errors.New("run cancelled")
	ErrModel              = errors.New("model error")
	ErrTool               = errors.New("tool error")
	ErrSession            = errors.New("session error")
	ErrPlugin             = errors.New("plugin error")
)

// Error is a wrapped public failure. Kind is one of the sentinel errors.
type Error struct {
	Kind error
	Op   string
	Err  error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := "agent"
	if e.Kind != nil {
		msg = e.Kind.Error()
	}
	if e.Op != "" {
		msg += " " + e.Op
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes both the sentinel kind and the cause.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	var out []error
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Err != nil {
		out = append(out, e.Err)
	}
	return out
}

// Wrap builds an Error of the given kind. A nil cause is allowed.
func Wrap(kind error, op string, cause error) error {
	if kind == nil && cause == nil {
		return nil
	}
	return &Error{Kind: kind, Op: op, Err: cause}
}

// AsKind reports whether err matches a public sentinel.
func AsKind(err, kind error) bool {
	return errors.Is(err, kind)
}

// Invalid wraps an invalid-config error.
func Invalid(op string, cause error) error {
	return Wrap(ErrInvalidConfig, op, cause)
}
