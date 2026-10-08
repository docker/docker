// Package prune defines events emitted by streaming prune operations.
package prune

// Action identifies a confirmed change to an object.
type Action string

const (
	// Deleted identifies a removed object.
	Deleted Action = "deleted"
	// Untagged identifies an image reference removed from an image.
	Untagged Action = "untagged"
)

// Object identifies a confirmed deletion or an image reference untagged.
// ID uses the same representation as the corresponding final report entry.
type Object struct {
	ID     string `json:"id"`
	Action Action `json:"action"`
}

// Error describes a prune operation that failed after streaming began.
type Error struct {
	Message string `json:"message"`
}

// Event contains exactly one object change, complete report, or error.
// A report terminates a successful stream.
type Event[T any] struct {
	Object *Object `json:"object,omitempty"`
	Report *T      `json:"report,omitempty"`
	Error  *Error  `json:"error,omitempty"`
}
