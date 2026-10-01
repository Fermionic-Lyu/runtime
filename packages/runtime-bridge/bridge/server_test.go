package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/runtime-bridge/backend"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

type testRuntime struct {
	mu        sync.Mutex
	instances map[string]backend.Instance
	onCreate  func(context.Context, backend.Spec) error
	onExec    func(context.Context, string, []string) (backend.Result, error)
	deleteErr error
	deletes   int
}

func (f *testRuntime) Create(ctx context.Context, spec backend.Spec) error {
	f.mu.Lock()
	f.instances[spec.Name] = backend.Instance{Name: spec.Name, State: "running"}
	f.mu.Unlock()
	if f.onCreate != nil {
		return f.onCreate(ctx, spec)
	}

	return nil
}

func (f *testRuntime) Get(_ context.Context, name string) (backend.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.instances[name]
	if !ok {
		return v, backend.ErrNotFound
	}

	return v, nil
}

func (f *testRuntime) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.instances, name)

	return nil
}

func (f *testRuntime) Exec(ctx context.Context, name string, args []string) (backend.Result, error) {
	if f.onExec != nil {
		return f.onExec(ctx, name, args)
	}

	return backend.Result{Stdout: "123\nhello\n", Stderr: "warning\n", ExitCode: 7}, nil
}

func newTestServer(t *testing.T) (*Server, *testRuntime) {
	t.Helper()
	f := &testRuntime{instances: map[string]backend.Instance{}}
	s, err := New(Config{StateFile: filepath.Join(t.TempDir(), "state.json"), TeamID: "team", TemplateID: "template", Image: "image", NodeID: "node", MaxSandboxes: 4, RuntimeIdentity: "test"}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s, f
}

func createRequest() *orchestrator.SandboxCreateRequest {
	id := "sandbox01"
	token := "secret-" + id

	return &orchestrator.SandboxCreateRequest{Sandbox: &orchestrator.SandboxConfig{SandboxId: id, ExecutionId: "execution", TeamId: "team", TemplateId: "template", EnvdAccessToken: &token, Vcpu: 1, RamMb: 512}, StartTime: timestamppb.Now(), EndTime: timestamppb.New(time.Now().Add(time.Minute))}
}

func TestSlowCreateDoesNotBlockNodeHealth(t *testing.T) {
	t.Parallel()
	s, f := newTestServer(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.onCreate = func(context.Context, backend.Spec) error {
		close(entered)
		<-release

		return nil
	}
	go func() { defer close(done); _, _ = s.Create(t.Context(), createRequest()) }()
	<-entered
	defer func() { close(release); <-done }()
	health := make(chan struct{})
	go func() { _, _ = s.ServiceInfo(t.Context(), &emptypb.Empty{}); close(health) }()
	select {
	case <-health:
	case <-time.After(time.Second):
		t.Fatal("node health blocked behind a pending backend create")
	}
}

func TestCleanupSurvivesStateWriteFailure(t *testing.T) {
	t.Parallel()
	s, f := newTestServer(t)
	f.onCreate = func(context.Context, backend.Spec) error {
		s.config.StateFile = filepath.Join(t.TempDir(), "missing", "state.json")

		return errors.New("create response lost")
	}
	_, err := s.Create(t.Context(), createRequest())
	if err == nil {
		t.Fatal("expected create failure")
	}
	if f.deletes != 1 {
		t.Fatalf("cleanup calls=%d, want 1 despite persistence failure", f.deletes)
	}
}

func TestRestartRetriesFailedDeletion(t *testing.T) {
	t.Parallel()
	s, f := newTestServer(t)
	req := createRequest()
	if _, err := s.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	f.deleteErr = errors.New("unavailable")
	if _, err := s.Delete(t.Context(), &orchestrator.SandboxDeleteRequest{SandboxId: "sandbox01"}); err == nil {
		t.Fatal("expected deletion failure")
	}
	_ = s.Close()
	restarted, err := New(s.config, f)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	f.deleteErr = nil
	if err := restarted.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.instances) != 0 || len(restarted.records) != 0 {
		t.Fatal("pending deletion was lost on restart")
	}
}

func TestRestartAndExpiration(t *testing.T) {
	t.Parallel()
	s, f := newTestServer(t)
	req := createRequest()
	if _, err := s.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	restarted, err := New(s.config, f)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Reconcile(t.Context()); err != nil || len(f.instances) != 1 {
		t.Fatalf("ready sandbox did not survive restart: %v", err)
	}
	restarted.records["sandbox01"].ExpiresAt = time.Now().Add(-time.Second)
	if err := restarted.Reconcile(t.Context()); err != nil || len(f.instances) != 0 {
		t.Fatalf("expired sandbox survived reconciliation: %v", err)
	}
}

func TestStateCannotMoveToAnotherRuntime(t *testing.T) {
	t.Parallel()
	s, f := newTestServer(t)
	if _, err := s.Create(t.Context(), createRequest()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	cfg := s.config
	cfg.RuntimeIdentity = "another-tenant"
	if other, err := New(cfg, f); err == nil {
		_ = other.Close()
		t.Fatal("state was accepted for another runtime")
	}
}

func TestUnsupportedCapabilitiesHaveNoSideEffects(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*orchestrator.SandboxConfig){
		func(c *orchestrator.SandboxConfig) { c.AutoPause = true },
		func(c *orchestrator.SandboxConfig) { c.Snapshot = true },
		func(c *orchestrator.SandboxConfig) { v := false; c.AllowInternetAccess = &v },
		func(c *orchestrator.SandboxConfig) {
			c.Network = &orchestrator.SandboxNetworkConfig{Egress: &orchestrator.SandboxNetworkEgressConfig{DeniedCidrs: []string{"0.0.0.0/0"}}}
		},
	} {
		s, f := newTestServer(t)
		req := createRequest()
		mutate(req.GetSandbox())
		_, err := s.Create(t.Context(), req)
		if status.Code(err) != codes.Unimplemented || len(f.instances) != 0 {
			t.Fatalf("unsupported request was not rejected: %v", err)
		}
	}
}

func TestIdentityAndIdempotency(t *testing.T) {
	t.Parallel()
	s, f := newTestServer(t)
	req := createRequest()
	if _, err := s.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	other := proto.Clone(req).(*orchestrator.SandboxCreateRequest)
	other.Sandbox.ExecutionId = "other"
	if _, err := s.Create(t.Context(), other); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	other.Sandbox.TeamId = "other-team"
	if _, err := s.Create(t.Context(), other); status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	if len(f.instances) != 1 {
		t.Fatal("duplicate runtime created")
	}
}

func TestProcessAuthAndExitCode(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	req := createRequest()
	if _, err := s.Create(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()
	client := processconnect.NewProcessClient(httpServer.Client(), httpServer.URL)
	start := connect.NewRequest(&process.StartRequest{Process: &process.ProcessConfig{Cmd: "sh", Args: []string{"-c", "echo hello"}}})
	start.Header().Set("E2b-Sandbox-Id", "sandbox01")
	start.Header().Set("X-Access-Token", req.GetSandbox().GetEnvdAccessToken())
	basic, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost", nil)
	basic.SetBasicAuth("root", "")
	start.Header().Set("Authorization", basic.Header.Get("Authorization"))
	stream, err := client.Start(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	var events []*process.ProcessEvent
	for stream.Receive() {
		events = append(events, stream.Msg().GetEvent())
	}
	if stream.Err() != nil {
		t.Fatal(stream.Err())
	}
	if len(events) != 4 || events[0].GetStart().GetPid() != 123 || string(events[1].GetData().GetStdout()) != "hello\n" || events[3].GetEnd().GetExitCode() != 7 {
		t.Fatalf("wrong process result: %v", events)
	}
	start.Header().Set("X-Access-Token", "wrong")
	bad, badErr := client.Start(t.Context(), start)
	if badErr == nil {
		for bad.Receive() {
		}
		badErr = bad.Err()
	}
	if badErr == nil {
		t.Fatal("unauthorized command executed")
	}
}
