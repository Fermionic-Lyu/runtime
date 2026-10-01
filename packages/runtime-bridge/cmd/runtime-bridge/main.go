package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/e2b-dev/infra/packages/runtime-bridge/bridge"
	"github.com/e2b-dev/infra/packages/runtime-bridge/compute"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	info "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	runtime, err := compute.New(compute.Config{URL: os.Getenv("INSTA_COMPUTE_URL"), Token: os.Getenv("INSTA_COMPUTE_TOKEN"), Tenant: os.Getenv("INSTA_COMPUTE_TENANT"), Branch: os.Getenv("INSTA_COMPUTE_BRANCH"), Region: os.Getenv("INSTA_COMPUTE_REGION")})
	if err != nil {
		return err
	}
	capacity, err := strconv.Atoi(value("BRIDGE_MAX_SANDBOXES", "4"))
	if err != nil {
		return err
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(os.Getenv("INSTA_COMPUTE_URL")+"\x00"+os.Getenv("INSTA_COMPUTE_TENANT")+"\x00"+os.Getenv("INSTA_COMPUTE_BRANCH"))))
	s, err := bridge.New(bridge.Config{StateFile: os.Getenv("BRIDGE_STATE_FILE"), TeamID: os.Getenv("BRIDGE_TEAM_ID"), TemplateID: value("BRIDGE_TEMPLATE_ID", "instapoc"), Image: os.Getenv("BRIDGE_IMAGE"), NodeID: value("BRIDGE_NODE_ID", "insta-poc"), MaxSandboxes: capacity, RuntimeIdentity: identity}, runtime)
	if err != nil {
		return err
	}
	defer s.Close()
	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 35*time.Second)
	err = s.Reconcile(reconcileCtx)
	reconcileCancel()
	if err != nil {
		return err
	}
	listenConfig := net.ListenConfig{}
	lis, err := listenConfig.Listen(ctx, "tcp", value("BRIDGE_GRPC_ADDRESS", "127.0.0.1:15008"))
	if err != nil {
		return err
	}
	defer lis.Close()
	grpcServer := grpc.NewServer()
	orchestrator.RegisterSandboxServiceServer(grpcServer, s)
	info.RegisterInfoServiceServer(grpcServer, s)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	httpServer := &http.Server{Addr: value("BRIDGE_HTTP_ADDRESS", "127.0.0.1:49983"), Handler: http.MaxBytesHandler(s, 1<<20), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 2)
	go func() { errCh <- grpcServer.Serve(lis) }()
	go func() { errCh <- httpServer.ListenAndServe() }()
	log.Printf("runtime bridge ready: grpc=%s process=%s", lis.Addr(), httpServer.Addr)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	running := true
	for running {
		select {
		case <-ctx.Done():
			running = false
		case err = <-errCh:
			running = false
		case <-tick.C:
			reconcileCtx, stop := context.WithTimeout(ctx, 35*time.Second)
			if e := s.Reconcile(reconcileCtx); e != nil {
				log.Printf("reconcile failed: %v", e)
			}
			stop()
		}
	}
	cancel()
	stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = httpServer.Shutdown(stopCtx)
	grpcServer.Stop()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}

	return err
}

func value(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}
