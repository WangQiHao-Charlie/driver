package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	runtimev1 "github.com/WangQiHao-Charlie/driver/api/proto/runtime/v1"
	"github.com/WangQiHao-Charlie/driver/internal/service"
	"github.com/WangQiHao-Charlie/driver/pkg/driver"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		sock  = flag.String("socket", "/var/run/driverd/runtime-driver.grpc", "unix socket path")
		feats = flag.String("features", "", "comma-separated feature list")
		vmode = flag.Bool("verbose", false, "enable verbose logging")
	)
	flag.Parse()

	if *vmode {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	// Remove existing socket file if any
	if err := os.MkdirAll(filepath.Dir(*sock), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	if _, err := os.Stat(*sock); err == nil {
		_ = os.Remove(*sock)
	}

	l, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer l.Close()
	_ = os.Chmod(*sock, 0o766)

	grpcServer := grpc.NewServer()
	defer grpcServer.GracefulStop()

	// Prepare service implementation
	var features []string
	if *feats != "" {
		features = append(features, splitComma(*feats)...)
	}
	echoPath, _ := exec.LookPath("echo")
	routes := map[string][]string{
		"echo": {echoPath, "{param:msg}", "{subject_id}"},
		// Outputs artifact JSON as the last stdout line
		// Example resolved: {"artifacts":{"snapshot":"/var/run/kuberisc/snaps/vm-1.img"}}
		"snapshot": {echoPath, "{\"artifacts\":{\"snapshot\":\"/var/run/kuberisc/snaps/{subject_id}.img\"}}"},
	}
	router := driver.NewCommandRouter(routes)
	impl := service.NewRuntimeDriverServer(
		driver.NewTemplateDriver(driver.Config{AllowedBinaries: []string{echoPath}}, router, 2*time.Second),
		features,
		map[string]string{"impl": "template"},
	)

	// Register service with gRPC server
	runtimev1.RegisterRuntimeDriverServer(grpcServer, impl)
	// Enable server reflection for grpcurl and other tools
	reflection.Register(grpcServer)

	// Signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go handleSignals(cancel)

	log.Printf("gRPC runtime driver listening on %s", *sock)

	// Serve in goroutine; wait for signal
	errc := make(chan error, 1)
	go func() { errc <- grpcServer.Serve(l) }()
	select {
	case <-ctx.Done():
		// Allow graceful stop below
	case err := <-errc:
		if err != nil {
			log.Printf("grpc serve error: %v", err)
		}
	}
	// Graceful stop, give in-flight RPCs a moment
	time.Sleep(100 * time.Millisecond)
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func handleSignals(cancel context.CancelFunc) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	fmt.Println("\nshutting down...")
	cancel()
}
