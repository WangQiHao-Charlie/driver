package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config controls the behavior of ExecDriver.
type Config struct {
    // Absolute paths of allowed binaries for argv[0]. Example: "/usr/local/bin/fc-control".
    AllowedBinaries []string

    // Optional path to a newline-separated allowlist file. Each non-empty,
    // non-comment line must be an absolute path to an allowed binary. Lines
    // beginning with '#' are treated as comments. If set, entries from this
    // file are UNIONed with AllowedBinaries.
    AllowedBinariesFile string

    // Minimum interval between allowlist file reload checks. If zero, defaults
    // to 2 seconds. The file is re-read only when its mtime changes.
    AllowedBinariesReload time.Duration

	// Concurrency control; number of concurrent execs.
	MaxConcurrency int

	// Tail sizes in bytes; default 8192 when zero.
	StdoutTailBytes int
	StderrTailBytes int

	// Timeout grace period between SIGTERM and SIGKILL when timing out/canceled.
	TerminationGrace time.Duration // default 5s

	// Idempotency cache size and TTL.
	CacheSize int           // default 64
	CacheTTL  time.Duration // default 5m

	// Artifact JSON output directory; if non-empty, attempt to read <dir>/<execution_id>.json.
	ArtifactOutDir string // default "/var/run/kuberisc/out"
}

type ExecDriver struct {
    cfg   Config
    sema  chan struct{}
    cache *respCache

    // metrics
    mActive   int64    // gauge
    mSuccess  uint64   // counter
    mDuration struct { // naive histogram: sum and count
        sumMicros uint64
        count     uint64
    }

    // dynamic allowlist (file-backed)
    allowMu        sync.RWMutex
    allowSet       map[string]struct{}
    allowLastMod   time.Time
    allowLastCheck time.Time
}

// NewExecDriver creates a Driver that executes local binaries safely.
func NewExecDriver(cfg Config) *ExecDriver {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	if cfg.StdoutTailBytes <= 0 {
		cfg.StdoutTailBytes = 8 << 10
	}
	if cfg.StderrTailBytes <= 0 {
		cfg.StderrTailBytes = 8 << 10
	}
    if cfg.TerminationGrace <= 0 {
        cfg.TerminationGrace = 5 * time.Second
    }
    if cfg.AllowedBinariesReload <= 0 {
        cfg.AllowedBinariesReload = 2 * time.Second
    }
    if cfg.CacheSize <= 0 {
        cfg.CacheSize = 64
    }
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.ArtifactOutDir == "" {
		cfg.ArtifactOutDir = "/var/run/kuberisc/out"
	}
    d := &ExecDriver{
        cfg:   cfg,
        sema:  make(chan struct{}, cfg.MaxConcurrency),
        cache: newRespCache(cfg.CacheSize, cfg.CacheTTL),
    }
    // Initialize allowlist cache if a file is configured
    if cfg.AllowedBinariesFile != "" {
        _ = d.reloadAllowlistIfNeeded(true)
    }
    return d
}

func (d *ExecDriver) Execute(ctx context.Context, req ExecReq) (ExecResp, error) {
	if req.ExecutionID != "" {
		if resp, ok := d.cache.Get(req.ExecutionID); ok {
			return resp, nil
		}
	}

	if len(req.Command) == 0 {
		return ExecResp{}, fmt.Errorf("empty command: provide rendered argv")
	}
	// Enforce allowlist for argv[0]. Resolve with LookPath only when the resolved path is in the allowlist.
	argv0 := req.Command[0]
	resolved := argv0
	if !filepath.IsAbs(argv0) {
		p, err := exec.LookPath(argv0)
		if err != nil {
			return ExecResp{}, fmt.Errorf("binary not found: %s", argv0)
		}
		resolved = p
	}
    if !d.isAllowedBinary(resolved) {
        return ExecResp{}, fmt.Errorf("binary not allowed: %s", resolved)
    }

	// Concurrency gate
	d.sema <- struct{}{}
	defer func() { <-d.sema }()

	start := time.Now()
	atomic.AddInt64(&d.mActive, 1)
	defer atomic.AddInt64(&d.mActive, -1)

	// Build command
	cmd := exec.Command(resolved, req.Command[1:]...)
	// Create its own process group to signal TERM/KILL to children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Controlled environment
	cmd.Env = []string{
		"KUBERISC_EXECUTION_ID=" + req.ExecutionID,
		"KUBERISC_SUBJECT_ID=" + req.SubjectID,
		"KUBERISC_INSTRUCTION=" + req.Instruction,
		"KUBERISC_NODE_NAME=" + req.NodeName,
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return ExecResp{}, fmt.Errorf("spawn stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return ExecResp{}, fmt.Errorf("spawn stderr pipe: %w", err)
	}

	// Capture tails and last stdout line for JSON artifacts.
	outTail := newTailBuffer(d.cfg.StdoutTailBytes)
	errTail := newTailBuffer(d.cfg.StderrTailBytes)
	stdoutLast := &lastLine{}

	// Start process
	d.logJSON(map[string]any{
		"event":        "exec.start",
		"instruction":  req.Instruction,
		"exec_id":      req.ExecutionID,
		"subject_id":   req.SubjectID,
		"argv0":        resolved,
		"args_len":     len(req.Command) - 1,
		"timeout_secs": int(req.Timeout / time.Second),
	})
	if err := cmd.Start(); err != nil {
		// SpawnError mapping
		d.logJSON(map[string]any{
			"event":       "exec.spawn_error",
			"instruction": req.Instruction,
			"exec_id":     req.ExecutionID,
			"error":       err.Error(),
		})
		return ExecResp{}, fmt.Errorf("spawn error: %w", err)
	}

	// Reader goroutines
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ioCopyMulti(stdoutPipe, outTail, stdoutLast)
	}()
	go func() {
		defer wg.Done()
		ioCopyMulti(stderrPipe, errTail)
	}()

	// Timeout & cancellation handling with TERM then KILL.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeout <-chan time.Time
	if req.Timeout > 0 {
		timer := time.NewTimer(req.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}

	var waitErr error
	select {
	case waitErr = <-done:
		// finished
	case <-ctx.Done():
		waitErr = d.terminateProcessGroup(cmd.Process.Pid)
	case <-timeout:
		waitErr = d.terminateProcessGroup(cmd.Process.Pid)
	}

	// ensure readers are drained
	wg.Wait()

	// Determine exit code
	exitCode := int32(-1)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	}
	// Collect artifacts
	artifacts := map[string]string{}
	// 1) last-line JSON
	if ll := stdoutLast.Get(); ll != "" {
		if m := parseArtifactsJSON(ll); len(m) > 0 {
			for k, v := range m {
				artifacts[k] = v
			}
		}
	}
	// 2) well-known output file
	if req.ExecutionID != "" {
		if m, err := readArtifactsFile(filepath.Join(d.cfg.ArtifactOutDir, req.ExecutionID+".json")); err == nil {
			for k, v := range m {
				artifacts[k] = v
			}
		}
	}

	resp := ExecResp{
		ExitCode:   exitCode,
		StdoutTail: outTail.String(),
		StderrTail: errTail.String(),
		Artifacts:  artifacts,
	}

	// Metrics
	dur := time.Since(start)
	atomic.AddUint64(&d.mDuration.count, 1)
	atomic.AddUint64(&d.mDuration.sumMicros, uint64(dur/time.Microsecond))
	if waitErr == nil && exitCode == 0 {
		atomic.AddUint64(&d.mSuccess, 1)
	}

	// Idempotency cache
	if req.ExecutionID != "" {
		d.cache.Put(req.ExecutionID, resp)
	}

	// Return error semantics: prefer process/timeout errors; otherwise nil even if exit_code != 0 (let caller map).
	d.logJSON(map[string]any{
		"event":       "exec.finish",
		"instruction": req.Instruction,
		"exec_id":     req.ExecutionID,
		"subject_id":  req.SubjectID,
		"exit_code":   exitCode,
		"duration_ms": int(dur / time.Millisecond),
		"timeout":     req.Timeout > 0,
		"error":       errString(waitErr),
	})

	if waitErr != nil {
		// Context timeout/cancel or kill path
		return resp, mapWaitErr(waitErr)
	}
	return resp, nil
}

func (d *ExecDriver) isAllowedBinary(path string) bool {
    // Normalize path to compare against allowlist set
    p := path
    if rp, err := filepath.EvalSymlinks(path); err == nil {
        p = rp
    }

    // Fast path: check static list
    for _, allowed := range d.cfg.AllowedBinaries {
        if allowed == p {
            return true
        }
    }

    // Dynamic file-backed allowlist
    if d.cfg.AllowedBinariesFile != "" {
        _ = d.reloadAllowlistIfNeeded(false)
        d.allowMu.RLock()
        _, ok := d.allowSet[p]
        d.allowMu.RUnlock()
        if ok {
            return true
        }
    }
    return false
}

// reloadAllowlistIfNeeded refreshes the file-backed allowlist if enough time
// has passed since the last check, and the file's mtime has changed.
func (d *ExecDriver) reloadAllowlistIfNeeded(force bool) error {
    if d.cfg.AllowedBinariesFile == "" {
        return nil
    }
    now := time.Now()
    d.allowMu.RLock()
    lastCheck := d.allowLastCheck
    d.allowMu.RUnlock()
    if !force && now.Sub(lastCheck) < d.cfg.AllowedBinariesReload {
        return nil
    }
    fi, err := os.Stat(d.cfg.AllowedBinariesFile)
    if err != nil {
        // If file missing temporarily, keep previous set
        d.allowMu.Lock()
        d.allowLastCheck = now
        d.allowMu.Unlock()
        return nil
    }
    modTime := fi.ModTime()
    d.allowMu.RLock()
    same := d.allowLastMod.Equal(modTime)
    d.allowMu.RUnlock()
    if !force && same {
        d.allowMu.Lock()
        d.allowLastCheck = now
        d.allowMu.Unlock()
        return nil
    }
    // Read and parse file
    b, err := os.ReadFile(d.cfg.AllowedBinariesFile)
    if err != nil {
        d.allowMu.Lock()
        d.allowLastCheck = now
        d.allowMu.Unlock()
        return nil
    }
    lines := strings.Split(string(b), "\n")
    m := make(map[string]struct{}, len(lines))
    for _, line := range lines {
        s := strings.TrimSpace(line)
        if s == "" || strings.HasPrefix(s, "#") {
            continue
        }
        // Inline comments: strip trailing ' #' occurrences
        if i := strings.Index(s, " #"); i >= 0 {
            s = strings.TrimSpace(s[:i])
            if s == "" {
                continue
            }
        }
        if !filepath.IsAbs(s) {
            // ignore non-absolute for safety
            continue
        }
        if rp, err := filepath.EvalSymlinks(s); err == nil {
            s = rp
        }
        m[s] = struct{}{}
    }
    d.allowMu.Lock()
    d.allowSet = m
    d.allowLastMod = modTime
    d.allowLastCheck = now
    d.allowMu.Unlock()
    return nil
}

func (d *ExecDriver) terminateProcessGroup(pid int) error {
	// Send SIGTERM to the process group, then SIGKILL after grace.
	// Negative PID targets the process group.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(d.cfg.TerminationGrace)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return fmt.Errorf("terminated by driver (timeout/cancel)")
}

// mapWaitErr collapses common error types.
func mapWaitErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return context.Canceled
	}
	return err
}

// --- Helpers: tail buffer, last line, artifacts parsing, cache ---

type tailBuffer struct {
	b    []byte
	size int
	mu   sync.Mutex
}

func newTailBuffer(n int) *tailBuffer {
	if n <= 0 {
		n = 8 << 10
	}
	return &tailBuffer{b: make([]byte, 0, n), size: n}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.size {
		// keep only last size bytes of p
		t.b = append(t.b[:0], p[len(p)-t.size:]...)
		return len(p), nil
	}
	// ensure capacity
	if len(t.b)+len(p) > t.size {
		drop := len(t.b) + len(p) - t.size
		t.b = t.b[drop:]
	}
	t.b = append(t.b, p...)
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// lastLine tracks the last newline-terminated line seen.
type lastLine struct {
	mu      sync.Mutex
	partial bytes.Buffer
	line    string // last completed line (ending with '\n')
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			l.line = l.partial.String()
			l.partial.Reset()
			continue
		}
		l.partial.WriteByte(b)
	}
	return len(p), nil
}

func (l *lastLine) Get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.partial.Len() > 0 {
		return strings.TrimSpace(l.partial.String())
	}
	return strings.TrimSpace(l.line)
}

// ioCopyMulti copies r to multiple writers; best-effort without short-circuit.
func ioCopyMulti(r io.Reader, ws ...io.Writer) {
	br := bufio.NewReader(r)
	buf := make([]byte, 32<<10)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			for _, w := range ws {
				_, _ = w.Write(buf[:n])
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// ignore
			}
			return
		}
	}
}

func parseArtifactsJSON(line string) map[string]string {
	type out struct {
		Artifacts map[string]string `json:"artifacts"`
	}
	var o out
	if err := json.Unmarshal([]byte(line), &o); err != nil {
		return nil
	}
	return o.Artifacts
}

func readArtifactsFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path) // best-effort cleanup
	return parseArtifactsJSON(string(b)), nil
}

// respCache: simple TTL cache with size limit.
type respCache struct {
	mu    sync.Mutex
	m     map[string]respEntry
	order []string
	cap   int
	ttl   time.Duration
}

type respEntry struct {
	at   time.Time
	resp ExecResp
}

func newRespCache(capacity int, ttl time.Duration) *respCache {
	return &respCache{m: make(map[string]respEntry, capacity), cap: capacity, ttl: ttl}
}

func (c *respCache) Get(k string) (ExecResp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return ExecResp{}, false
	}
	if time.Since(e.at) > c.ttl {
		delete(c.m, k)
		return ExecResp{}, false
	}
	return e.resp, true
}

func (c *respCache) Put(k string, v ExecResp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[k]; !ok {
		c.order = append(c.order, k)
	}
	c.m[k] = respEntry{at: time.Now(), resp: v}
	// Evict when over capacity
	for len(c.m) > c.cap && len(c.order) > 0 {
		victim := c.order[0]
		c.order = c.order[1:]
		delete(c.m, victim)
	}
}

// Metrics exposes a snapshot of internal counters.
type Metrics struct {
	Active            int64
	Success           uint64
	DurationCount     uint64
	DurationSumMicros uint64
}

func (d *ExecDriver) Metrics() Metrics {
	return Metrics{
		Active:            atomic.LoadInt64(&d.mActive),
		Success:           atomic.LoadUint64(&d.mSuccess),
		DurationCount:     atomic.LoadUint64(&d.mDuration.count),
		DurationSumMicros: atomic.LoadUint64(&d.mDuration.sumMicros),
	}
}

func (d *ExecDriver) logJSON(m map[string]any) {
	// Lightweight structured log; avoid leaking params.
	m["ts"] = time.Now().Format(time.RFC3339Nano)
	if _, ok := m["level"]; !ok {
		m["level"] = "info"
	}
	b, _ := json.Marshal(m)
	// Write to stdout to keep library self-contained.
	_, _ = os.Stdout.Write(append(b, '\n'))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
