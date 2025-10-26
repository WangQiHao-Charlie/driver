**Overview**
- Implements a gRPC RuntimeDriver service in Go.
- Exposes Execute and Discover, bridging to your Driver interface.
- Includes a WASI sandbox runner (`pkg/wasi`) and a demo CLI (`cmd/wasictl`).

**Layout**
- `api/proto/runtime/v1/driver.proto`: Service + messages (also codegen output).
- `pkg/driver/driver.go`: Adapter interface `Driver`, `ExecReq`, `ExecResp`.
- `internal/service/runtime_driver.go`: gRPC service adapter that delegates to `Driver`.
- `pkg/driver/nop.go`: Minimal no-op driver implementation.
- `cmd/driverd/main.go`: Example daemon serving gRPC over a Unix socket.

**Generate Code**
- Requires `protoc`, `protoc-gen-go` and `protoc-gen-go-grpc` in your PATH.
- Install tools (example):
  - `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`
  - `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`
- Generate to `api/gen` with source-relative paths:
  - `protoc -I . \
      --go_out=. \
      --go-grpc_out=. \
      api/proto/runtime/v1/driver.proto`

This creates Go code in `api/proto/runtime/v1` (import path `hackohio/driver/api/proto/runtime/v1`).

**Run the Daemon**
- `go run ./cmd/driverd -socket /tmp/runtime-driver.grpc -features snapshot,foo`
- Optional: provide an external allowlist file for permitted binaries:
  - `go run ./cmd/driverd -socket /tmp/runtime-driver.grpc -allowlist /etc/kuberisc/driver.allow`

**Implement Your Driver**
- Use the provided exec-based driver: `pkg/driver.NewExecDriver(driver.Config{...})`.
- Contract: `Execute(ctx, ExecReq) (ExecResp, error)` where `ExecReq.Command` is a fully-rendered `[]string` (no shell), plus `ExecutionID`, `Timeout`, etc.
- Security: binary allowlist, no shell, minimal env (`KUBERISC_*` only).
- Concurrency: semaphore with configurable `MaxConcurrency`.
- Idempotency: in-memory LRU+TTL cache keyed by `ExecutionID`.
- Output: keeps last 8KB of stdout/stderr and parses artifacts from last-line JSON or `/var/run/kuberisc/out/<execution_id>.json`.

**Client Notes**
- Use the generated gRPC client `runtimev1.NewRuntimeDriverClient` over a `*grpc.ClientConn`.
- Unix socket example (Go):
  - `conn, _ := grpc.Dial(
        "unix:///tmp/runtime-driver.grpc",
        grpc.WithTransportCredentials(insecure.NewCredentials()),
        grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
            return net.Dial("unix", "/tmp/runtime-driver.grpc")
        }),
      )`
  - `c := runtimev1.NewRuntimeDriverClient(conn)`
  - `c.Execute(ctx, &runtimev1.ExecuteRequest{...})`
  - `c.Discover(ctx, &emptypb.Empty{})`

**Message Mapping**
- ExecuteRequest → ExecReq fields: instruction, subject_id, params, execution_id.
- ExecuteReply ← ExecResp fields: exit_code, stdout_tail, stderr_tail, artifacts.

**Exec Driver Usage (direct call)**
- Example:
  - `d := driver.NewExecDriver(driver.Config{AllowedBinaries: []string{"/usr/bin/ctr"}})`
  - `resp, err := d.Execute(ctx, driver.ExecReq{Command: []string{"/usr/bin/ctr","images","ls"}, Instruction: "list", SubjectID: "node1", ExecutionID: "abc-123", Timeout: 10*time.Second, NodeName: "node1"})`
  - `resp.ExitCode`, `resp.StdoutTail`, `resp.StderrTail`, `resp.Artifacts`

**External Allowlist**
- You can manage allowed binaries without recompiling by using a file-based allowlist.
- Driver flag: `-allowlist /path/to/file`.
- File format:
  - One absolute path per line, e.g. `/usr/bin/ctr`
  - Empty lines and lines starting with `#` are ignored
  - Trailing inline comments after ` #` are ignored
  - Symlinks are resolved for comparison
- Behavior:
  - Entries from the file are unioned with the static `AllowedBinaries` slice
  - The file is checked every ~2s and reloaded when its mtime changes
  - If the file is temporarily missing or unreadable, the previous set is kept

**WASI Sandbox Example**
- Runner: `pkg/wasi` provides `RunModule(ctx, wasi.Config)` to execute WASI modules in a preopened, sandboxed FS with env/args and tail capture.
- CLI: `go run ./cmd/wasictl -module testdata/hello.wasm -mount /tmp/wasi:/sandbox -args "--do,write" -exec-id exec-1 -timeout 5s`
- Result prints JSON: `exit_code`, `stdout_tail`, `stderr_tail`, `artifacts`.

WASI build example (TinyGo):
- Create `wasi_demo/main.go`:
  
  package main
  
  import (
      "fmt"
      "os"
      "path/filepath"
  )
  
  func main() {
      root := "/sandbox"
      _ = os.MkdirAll(filepath.Join(root, "data"), 0o755)
      // Write an artifacts file (optional)
      execID := os.Getenv("KUBERISC_EXECUTION_ID")
      if execID != "" {
        _ = os.MkdirAll("/var/run/kuberisc/out", 0o755)
        _ = os.WriteFile("/var/run/kuberisc/out/"+execID+".json", []byte(`{"artifacts":{"snapshot":"/sandbox/data/snap"}}`), 0o644)
      }
      // Also print last-line JSON for artifact parsing
      fmt.Println("hello from wasi")
      fmt.Println(`{"artifacts":{"snapshot":"/sandbox/data/snap"}}`)
  }

- Build (requires TinyGo):
  - `tinygo build -o testdata/hello.wasm -target=wasi wasi_demo/main.go`
- Run with sandbox mount:
  - `mkdir -p /tmp/wasi && go run ./cmd/wasictl -module testdata/hello.wasm -mount /tmp/wasi:/sandbox -exec-id exec-1`
- Expected output includes `artifacts.snapshot=/sandbox/data/snap`.
