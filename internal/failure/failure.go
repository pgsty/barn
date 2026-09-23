// Package failure attaches a user-facing class, a stable reason, and a next
// action to an error. The command boundary maps the class to an exit code;
// inner packages tag only what they know for certain, and anything untagged
// is a runtime failure.
package failure

// Class is the closed set of failure categories shown to users and scripts.
type Class string

const (
	Runtime    Class = "runtime"    // the operation ran and failed
	Usage      Class = "usage"      // the command line or inventory is wrong
	Capability Class = "capability" // the host lacks a tool, network, or privilege
	Conflict   Class = "conflict"   // the current deployment state forbids it
	Partial    Class = "partial"    // some nodes succeeded and some failed
	Resource   Class = "resource"   // a host address, port, subnet, or disk is taken
	Integrity  Class = "integrity"  // a verified digest, identity, or ownership did not match
	Cancelled  Class = "cancelled"  // the user interrupted or declined
)

// Error carries classification alongside an underlying error. Its message is
// the underlying message; classification never changes the wording.
type Error struct {
	Class  Class
	Reason string
	Next   string
	Err    error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// New classifies err.
func New(class Class, err error) *Error { return &Error{Class: class, Err: err} }

// Because sets a stable snake_case reason for automation.
func (e *Error) Because(reason string) *Error { e.Reason = reason; return e }

// Then sets the next action shown to the user.
func (e *Error) Then(next string) *Error { e.Next = next; return e }

// WithNext attaches a next action without classifying err.
func WithNext(err error, next string) error {
	if err == nil {
		return nil
	}
	return &Error{Err: err, Next: next}
}

// Classify walks the error tree outermost first. The first classified node
// decides the class and reason; the first non-empty next action wins.
func Classify(err error) (class Class, reason, next string) {
	walk(err, func(node *Error) {
		if class == "" && node.Class != "" {
			class, reason = node.Class, node.Reason
		}
		if next == "" {
			next = node.Next
		}
	})
	return class, reason, next
}

func walk(err error, visit func(*Error)) {
	if err == nil {
		return
	}
	if node, ok := err.(*Error); ok {
		visit(node)
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			walk(child, visit)
		}
	case interface{ Unwrap() error }:
		walk(wrapped.Unwrap(), visit)
	}
}
