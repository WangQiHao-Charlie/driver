package driver

import (
	"context"
	"fmt"
)

// Nop implements Driver with a no-op success.
type Nop struct{}

func (Nop) Execute(ctx context.Context, req ExecReq) (ExecResp, error) {
	return ExecResp{
		ExitCode:   0,
		StdoutTail: fmt.Sprintf("noop:%s", req.Instruction),
		StderrTail: "",
		Artifacts:  map[string]string{"status": "ok"},
	}, nil
}
