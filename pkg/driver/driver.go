package driver

import (
	"context"
	"time"
)

// ExecReq is the adapter-layer request for Execute.
// For MVP, the caller provides a fully rendered command (no shell).
type ExecReq struct {
	// Fully rendered command; argv[0] must be in the allowlist.
	Command []string

	Instruction string
	SubjectID   string
	Params      map[string]string
	ExecutionID string

	// Optional execution context
	Timeout     time.Duration // zero => no explicit timeout
	NodeName    string        // for env injection
	Annotations map[string]string
}

// ExecResp is the adapter-layer response for Execute.
type ExecResp struct {
	ExitCode   int32
	StdoutTail string
	StderrTail string
	Artifacts  map[string]string
}

// Driver defines the exec adapter interface.
type Driver interface {
	Execute(ctx context.Context, req ExecReq) (ExecResp, error)
}
