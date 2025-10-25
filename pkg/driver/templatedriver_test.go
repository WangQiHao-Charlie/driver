package driver_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	d "github.com/WangQiHao-Charlie/driver/pkg/driver"
)

func lookupOrSkip(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found in PATH; skipping", name)
	}
	if !filepath.IsAbs(p) {
		t.Skipf("%s resolved to non-absolute path %q; skipping", name, p)
	}
	return p
}

func TestTemplateDriver_EchoRoute(t *testing.T) {
	echo := lookupOrSkip(t, "echo")

	routes := map[string][]string{
		"echo": {echo, "{param:msg}", "{subject_id}"},
	}
	router := d.NewCommandRouter(routes)
	drv := d.NewTemplateDriver(d.Config{AllowedBinaries: []string{echo}}, router, 2*time.Second)

	resp, err := drv.Execute(context.Background(), d.ExecReq{
		Instruction: "echo",
		SubjectID:   "subj-1",
		Params:      map[string]string{"msg": "hello"},
		ExecutionID: "tx-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", resp.ExitCode)
	}
	want := "hello subj-1\n"
	if resp.StdoutTail != want {
		t.Fatalf("stdout tail = %q, want %q", resp.StdoutTail, want)
	}
}

func TestTemplateDriver_NoRoute(t *testing.T) {
	echo := lookupOrSkip(t, "echo")
	router := d.NewCommandRouter(map[string][]string{})
	drv := d.NewTemplateDriver(d.Config{AllowedBinaries: []string{echo}}, router, 0)

	_, err := drv.Execute(context.Background(), d.ExecReq{Instruction: "not-exist"})
	if err == nil {
		t.Fatalf("expected error for missing route, got nil")
	}
}
