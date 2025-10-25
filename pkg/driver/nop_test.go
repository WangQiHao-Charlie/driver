package driver_test

import (
	"context"
	"testing"

	d "hackohio/driver/pkg/driver"
)

func TestNopExecute(t *testing.T) {
	var drv d.Driver = d.Nop{}
	resp, err := drv.Execute(context.Background(), d.ExecReq{Instruction: "test-nop", ExecutionID: "id-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
	if resp.StderrTail != "" {
		t.Fatalf("stderr_tail = %q, want empty", resp.StderrTail)
	}
	if resp.StdoutTail == "" || resp.StdoutTail != "noop:test-nop" {
		t.Fatalf("stdout_tail = %q, want 'noop:test-nop'", resp.StdoutTail)
	}
	if resp.Artifacts["status"] != "ok" {
		t.Fatalf("artifacts[status] = %q, want 'ok'", resp.Artifacts["status"])
	}
}
