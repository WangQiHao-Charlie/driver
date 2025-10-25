package service

import (
	"context"

	runtimev1 "hackohio/driver/api/proto/runtime/v1"
	"hackohio/driver/pkg/driver"

	"google.golang.org/protobuf/types/known/emptypb"
)

// RuntimeDriverServer adapts Driver to the gRPC generated service.
type RuntimeDriverServer struct {
	impl driver.Driver
	// Optional discovery data
	Features []string
	Metadata map[string]string
	runtimev1.UnimplementedRuntimeDriverServer
}

func NewRuntimeDriverServer(impl driver.Driver, features []string, metadata map[string]string) *RuntimeDriverServer {
	return &RuntimeDriverServer{impl: impl, Features: features, Metadata: metadata}
}

// Execute bridges the gRPC request to the Driver interface.
func (s *RuntimeDriverServer) Execute(ctx context.Context, req *runtimev1.ExecuteRequest) (*runtimev1.ExecuteReply, error) {
	if req == nil {
		req = &runtimev1.ExecuteRequest{}
	}
	resp, err := s.impl.Execute(ctx, driver.ExecReq{
		Instruction: req.GetInstruction(),
		SubjectID:   req.GetSubjectId(),
		Params:      req.GetParams(),
		ExecutionID: req.GetExecutionId(),
	})
	if err != nil {
		return nil, err
	}
	return &runtimev1.ExecuteReply{
		ExitCode:   resp.ExitCode,
		StdoutTail: resp.StdoutTail,
		StderrTail: resp.StderrTail,
		Artifacts:  resp.Artifacts,
	}, nil
}

// Discover returns static capabilities.
func (s *RuntimeDriverServer) Discover(ctx context.Context, _ *emptypb.Empty) (*runtimev1.Capabilities, error) {
	return &runtimev1.Capabilities{Features: s.Features, Metadata: s.Metadata}, nil
}
