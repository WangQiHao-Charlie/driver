package driver

import (
    "context"
    "fmt"
    "strings"
    "time"
)

// RouterFunc translates an instruction + context into a fully-rendered argv.
// The returned slice must include argv[0].
type RouterFunc func(instruction, subjectID string, params map[string]string, executionID string) ([]string, error)

// TemplateDriver composes ExecDriver with a routing function that renders
// concrete commands from high-level instructions and parameters.
//
// Typical flow:
//  1) service calls Execute with Instruction/SubjectID/Params
//  2) router produces []string{argv0, ...}
//  3) delegate to ExecDriver.Execute with the rendered Command
type TemplateDriver struct {
    exec           *ExecDriver
    router         RouterFunc
    defaultTimeout time.Duration
}

// NewTemplateDriver constructs a TemplateDriver with the given ExecDriver
// configuration and routing function. defaultTimeout is used only when the
// incoming request has zero Timeout and no parsable timeout in params.
func NewTemplateDriver(execCfg Config, router RouterFunc, defaultTimeout time.Duration) *TemplateDriver {
    return &TemplateDriver{
        exec:           NewExecDriver(execCfg),
        router:         router,
        defaultTimeout: defaultTimeout,
    }
}

// Execute implements Driver by resolving the command via router and delegating
// to the underlying ExecDriver.
func (t *TemplateDriver) Execute(ctx context.Context, req ExecReq) (ExecResp, error) {
    if t.router == nil {
        return ExecResp{}, fmt.Errorf("no router configured")
    }

    argv, err := t.router(req.Instruction, req.SubjectID, req.Params, req.ExecutionID)
    if err != nil {
        return ExecResp{}, err
    }
    if len(argv) == 0 {
        return ExecResp{}, fmt.Errorf("router produced empty command")
    }

    // Build downstream request. Preserve original metadata.
    outReq := ExecReq{
        Command:     argv,
        Instruction: req.Instruction,
        SubjectID:   req.SubjectID,
        Params:      req.Params,
        ExecutionID: req.ExecutionID,
        Timeout:     req.Timeout,
        NodeName:    req.NodeName,
        Annotations: req.Annotations,
    }

    // Apply timeout override logic if not set by caller.
    if outReq.Timeout == 0 {
        if d, ok := parseTimeoutParam(req.Params); ok {
            outReq.Timeout = d
        } else if t.defaultTimeout > 0 {
            outReq.Timeout = t.defaultTimeout
        }
    }

    return t.exec.Execute(ctx, outReq)
}

// NewCommandRouter creates a simple template-based RouterFunc.
// routes maps instruction -> command template (argv tokens).
// Supported placeholders inside tokens:
//  - {instruction}
//  - {subject_id}
//  - {execution_id}
//  - {param:KEY}
// Unknown placeholders are left as-is.
func NewCommandRouter(routes map[string][]string) RouterFunc {
    return func(instruction, subjectID string, params map[string]string, executionID string) ([]string, error) {
        tmpl, ok := routes[instruction]
        if !ok {
            return nil, fmt.Errorf("no route for instruction %q", instruction)
        }
        out := make([]string, 0, len(tmpl))
        for _, tok := range tmpl {
            out = append(out, expandToken(tok, instruction, subjectID, executionID, params))
        }
        return out, nil
    }
}

func expandToken(tok, instruction, subjectID, executionID string, params map[string]string) string {
    // Basic replacements
    r := strings.NewReplacer(
        "{instruction}", instruction,
        "{subject_id}", subjectID,
        "{execution_id}", executionID,
    )
    out := r.Replace(tok)

    // {param:KEY} substitutions (repeat until none)
    for {
        i := strings.Index(out, "{param:")
        if i < 0 {
            break
        }
        j := strings.Index(out[i:], "}")
        if j < 0 {
            break // unclosed; leave as-is
        }
        j = i + j
        key := out[i+7 : j] // between {param: and }
        val := ""
        if params != nil {
            if v, ok := params[key]; ok {
                val = v
            }
        }
        out = out[:i] + val + out[j+1:]
    }
    return out
}

// parseTimeoutParam checks common keys for duration, returns (d,true) if set.
// Recognized keys: "timeout", "timeout_s" (as integer seconds), "timeout_ms".
func parseTimeoutParam(m map[string]string) (time.Duration, bool) {
    if m == nil {
        return 0, false
    }
    if s, ok := m["timeout"]; ok && s != "" {
        if d, err := time.ParseDuration(s); err == nil {
            return d, true
        }
    }
    if s, ok := m["timeout_s"]; ok && s != "" {
        if d, err := parseIntSeconds(s); err == nil {
            return d, true
        }
    }
    if s, ok := m["timeout_ms"]; ok && s != "" {
        if d, err := parseIntMillis(s); err == nil {
            return d, true
        }
    }
    return 0, false
}

func parseIntSeconds(s string) (time.Duration, error) {
    var n int64
    for _, c := range s {
        if c < '0' || c > '9' {
            return 0, fmt.Errorf("invalid seconds: %q", s)
        }
        n = n*10 + int64(c-'0')
    }
    return time.Duration(n) * time.Second, nil
}

func parseIntMillis(s string) (time.Duration, error) {
    var n int64
    for _, c := range s {
        if c < '0' || c > '9' {
            return 0, fmt.Errorf("invalid millis: %q", s)
        }
        n = n*10 + int64(c-'0')
    }
    return time.Duration(n) * time.Millisecond, nil
}

