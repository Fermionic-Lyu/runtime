package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/e2b-dev/infra/packages/runtime-bridge/backend"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	info "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

type Config struct {
	StateFile       string
	TeamID          string
	TemplateID      string
	Image           string
	NodeID          string
	MaxSandboxes    int
	RuntimeIdentity string
}

type state struct {
	RuntimeIdentity string             `json:"runtime_identity"`
	Records         map[string]*record `json:"records"`
}

type record struct {
	Request   *orchestrator.SandboxCreateRequest `json:"request"`
	Name      string                             `json:"name"`
	Phase     string                             `json:"phase"`
	ExpiresAt time.Time                          `json:"expires_at"`
}

type Server struct {
	orchestrator.UnimplementedSandboxServiceServer
	info.UnimplementedInfoServiceServer

	mu         sync.Mutex
	operations sync.Map
	config     Config
	runtime    backend.Runtime
	records    map[string]*record
	lock       *os.File
	started    time.Time
}

func New(config Config, runtime backend.Runtime) (*Server, error) {
	if config.StateFile == "" || config.TeamID == "" || config.TemplateID == "" || config.Image == "" || config.NodeID == "" || config.MaxSandboxes < 1 || config.RuntimeIdentity == "" {
		return nil, errors.New("state file, team, template, image, node and positive capacity are required")
	}
	if err := os.MkdirAll(filepath.Dir(config.StateFile), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(config.StateFile+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()

		return nil, errors.New("bridge state is already in use")
	}
	s := &Server{config: config, runtime: runtime, records: map[string]*record{}, lock: lock, started: time.Now()}
	data, err := os.ReadFile(config.StateFile)
	if err == nil {
		var saved state
		err = json.Unmarshal(data, &saved)
		if err == nil && (saved.RuntimeIdentity != config.RuntimeIdentity || saved.Records == nil) {
			err = errors.New("bridge state belongs to another runtime configuration")
		}
		if err == nil {
			s.records = saved.Records
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Close()

		return nil, err
	}
	for id, rec := range s.records {
		if rec == nil || rec.Request == nil || rec.Request.GetSandbox() == nil || rec.Request.GetSandbox().GetSandboxId() != id || rec.Name != "e2b-"+id || rec.Request.GetSandbox().GetTeamId() != config.TeamID || rec.ExpiresAt.IsZero() || rec.Request.GetStartTime() == nil || (rec.Phase != "creating" && rec.Phase != "running" && rec.Phase != "deleting") {
			s.Close()

			return nil, errors.New("invalid bridge state or changed team")
		}
	}

	return s, nil
}

func (s *Server) Close() error { return s.lock.Close() }

func (s *Server) operation(id string) *sync.Mutex {
	lock, _ := s.operations.LoadOrStore(id, &sync.Mutex{})

	return lock.(*sync.Mutex)
}

func (s *Server) save() error {
	data, err := json.Marshal(state{RuntimeIdentity: s.config.RuntimeIdentity, Records: s.records})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.config.StateFile), ".bridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), s.config.StateFile); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.config.StateFile))
	if err != nil {
		return err
	}
	defer dir.Close()

	return dir.Sync()
}

func validate(req *orchestrator.SandboxCreateRequest, cfg Config) error {
	if req == nil || req.GetSandbox() == nil {
		return status.Error(codes.InvalidArgument, "sandbox is required")
	}
	b := req.GetSandbox()
	if !regexp.MustCompile(`^[a-z0-9]{8,32}$`).MatchString(b.GetSandboxId()) || b.GetExecutionId() == "" || b.GetVcpu() <= 0 || b.GetRamMb() <= 0 || req.GetStartTime() == nil || req.GetStartTime().CheckValid() != nil || req.GetEndTime() == nil || req.GetEndTime().CheckValid() != nil || !req.GetEndTime().AsTime().After(req.GetStartTime().AsTime()) || !req.GetEndTime().AsTime().After(time.Now()) {
		return status.Error(codes.InvalidArgument, "invalid sandbox identity, resources or deadline")
	}
	if b.GetTeamId() != cfg.TeamID {
		return status.Error(codes.PermissionDenied, "team is not configured for this bridge")
	}
	if b.GetTemplateId() != cfg.TemplateID {
		return status.Error(codes.NotFound, "template has no runtime image mapping")
	}
	if b.GetEnvdAccessToken() == "" {
		return status.Error(codes.InvalidArgument, "this bridge requires secure=true")
	}
	if b.GetSnapshot() || b.GetAutoPause() || b.GetAutoResume() != nil || req.GetFilesystemBoot() || len(b.GetVolumeMounts()) > 0 || b.GetIam() != nil {
		return status.Error(codes.Unimplemented, "snapshots, auto-pause/resume, volumes and workload identity are unsupported")
	}
	if b.AllowInternetAccess != nil && !b.GetAllowInternetAccess() {
		return status.Error(codes.Unimplemented, "network isolation is unsupported")
	}
	if n := b.GetNetwork(); n != nil {
		if e := n.GetEgress(); e != nil && proto.Size(e) > 0 {
			return status.Error(codes.Unimplemented, "egress policies are unsupported")
		}
		if i := n.GetIngress(); i != nil && proto.Size(i) > 0 {
			return status.Error(codes.Unimplemented, "ingress policies are unsupported")
		}
	}

	return nil
}

func (s *Server) Create(ctx context.Context, req *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	if err := validate(req, s.config); err != nil {
		return nil, err
	}
	id := req.GetSandbox().GetSandboxId()
	op := s.operation(id)
	op.Lock()
	defer op.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.records[id]; ok {
		if !proto.Equal(rec.Request, req) {
			return nil, status.Error(codes.AlreadyExists, "sandbox identity already has another request")
		}
		s.mu.Unlock()
		instance, err := s.runtime.Get(ctx, rec.Name)
		s.mu.Lock()
		if err == nil && rec.Phase == "running" && instance.State == "running" {
			return &orchestrator.SandboxCreateResponse{ClientId: s.config.NodeID}, nil
		}

		return nil, status.Error(codes.FailedPrecondition, "previous create has not completed; delete before retrying")
	}
	if len(s.records) >= s.config.MaxSandboxes {
		return nil, status.Error(codes.ResourceExhausted, "bridge capacity exhausted")
	}
	rec := &record{Request: proto.Clone(req).(*orchestrator.SandboxCreateRequest), Name: "e2b-" + id, Phase: "creating", ExpiresAt: req.GetEndTime().AsTime()}
	s.mu.Unlock()
	_, existingErr := s.runtime.Get(ctx, rec.Name)
	s.mu.Lock()
	if err := existingErr; !errors.Is(err, backend.ErrNotFound) {
		if err == nil {
			return nil, status.Error(codes.AlreadyExists, "runtime name is already occupied")
		}

		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if len(s.records) >= s.config.MaxSandboxes {
		return nil, status.Error(codes.ResourceExhausted, "bridge capacity exhausted")
	}
	s.records[id] = rec
	if err := s.save(); err != nil {
		delete(s.records, id)

		return nil, status.Error(codes.Internal, err.Error())
	}
	s.mu.Unlock()
	err := s.runtime.Create(ctx, backend.Spec{Name: rec.Name, Image: s.config.Image, Env: req.GetSandbox().GetEnvVars(), CPU: req.GetSandbox().GetVcpu(), MemoryMiB: req.GetSandbox().GetRamMb()})
	s.mu.Lock()
	if errors.Is(err, backend.ErrConflict) {
		delete(s.records, id)
		if saveErr := s.save(); saveErr != nil {
			return nil, status.Error(codes.Internal, saveErr.Error())
		}

		return nil, status.Error(codes.AlreadyExists, "runtime name is already occupied")
	}
	if err != nil {
		rec.Phase = "deleting"
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cleanupErr := s.remove(cleanupCtx, id)

		return nil, status.Error(codes.Unavailable, fmt.Sprintf("runtime create failed: %v; cleanup: %v", err, cleanupErr))
	}
	rec.Phase = "running"
	rec.ExpiresAt = time.Now().Add(req.GetEndTime().AsTime().Sub(req.GetStartTime().AsTime()))
	if err := s.save(); err != nil {
		rec.Phase = "deleting"

		return nil, status.Error(codes.Internal, err.Error())
	}

	return &orchestrator.SandboxCreateResponse{ClientId: s.config.NodeID}, nil
}

func (s *Server) remove(ctx context.Context, id string) error {
	rec, ok := s.records[id]
	if !ok {
		return nil
	}
	rec.Phase = "deleting"
	phaseErr := s.save()
	s.mu.Unlock()
	deleteErr := s.runtime.Delete(ctx, rec.Name)
	s.mu.Lock()
	if deleteErr != nil {
		return errors.Join(phaseErr, deleteErr)
	}
	delete(s.records, id)
	if err := s.save(); err != nil {
		s.records[id] = rec

		return err
	}

	return nil
}

func (s *Server) Delete(ctx context.Context, req *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	op := s.operation(req.GetSandboxId())
	op.Lock()
	defer op.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox ID is required")
	}
	if err := s.remove(ctx, req.GetSandboxId()); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	return &emptypb.Empty{}, nil
}

func (s *Server) Update(_ context.Context, req *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	op := s.operation(req.GetSandboxId())
	op.Lock()
	defer op.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[req.GetSandboxId()]
	if !ok || rec.Phase != "running" {
		return nil, status.Error(codes.NotFound, "sandbox is not running")
	}
	if req.GetEgress() != nil {
		return nil, status.Error(codes.Unimplemented, "network updates are unsupported")
	}
	if req.GetEndTime() != nil {
		if req.GetEndTime().CheckValid() != nil || !req.GetEndTime().AsTime().After(time.Now()) {
			return nil, status.Error(codes.InvalidArgument, "invalid deadline")
		}
		previous := rec.ExpiresAt
		rec.ExpiresAt = req.GetEndTime().AsTime()
		if err := s.save(); err != nil {
			rec.ExpiresAt = previous

			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &emptypb.Empty{}, nil
}

func (s *Server) List(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	s.mu.Lock()
	records := make(map[string]*record, len(s.records))
	for id, rec := range s.records {
		records[id] = &record{Name: rec.Name, Phase: rec.Phase, ExpiresAt: rec.ExpiresAt, Request: proto.Clone(rec.Request).(*orchestrator.SandboxCreateRequest)}
	}
	s.mu.Unlock()
	out := &orchestrator.SandboxListResponse{}
	for id, rec := range records {
		if rec.Phase != "running" {
			continue
		}
		instance, err := s.runtime.Get(ctx, rec.Name)
		if errors.Is(err, backend.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		if instance.State != "running" {
			continue
		}
		b := rec.Request.GetSandbox()
		out.Sandboxes = append(out.Sandboxes, &orchestrator.RunningSandbox{SandboxId: id, TeamId: b.GetTeamId(), ExecutionId: b.GetExecutionId(), ClientId: s.config.NodeID, Vcpu: b.GetVcpu(), RamMb: b.GetRamMb(), StartTime: rec.Request.GetStartTime(), EndTime: timestamppb.New(rec.ExpiresAt)})
	}

	return out, nil
}

func (s *Server) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.records))
	for id := range s.records {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var errs []error
	for _, id := range ids {
		op := s.operation(id)
		if !op.TryLock() {
			continue
		}
		s.mu.Lock()
		rec, ok := s.records[id]
		if ok && (rec.Phase != "running" || !rec.ExpiresAt.After(time.Now())) {
			if err := s.remove(ctx, id); err != nil {
				errs = append(errs, err)
			}
		}
		s.mu.Unlock()
		op.Unlock()
	}

	return errors.Join(errs...)
}

func (s *Server) ServiceInfo(_ context.Context, _ *emptypb.Empty) (*info.ServiceInfoResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &info.ServiceInfoResponse{NodeId: s.config.NodeID, ServiceId: s.config.NodeID, ServiceVersion: "0.0.0-insta-poc", ServiceStatus: info.ServiceInfoStatus_Healthy, ServiceRoles: []info.ServiceInfoRole{info.ServiceInfoRole_Orchestrator}, ServiceStartup: timestamppb.New(s.started), MaxSandboxes: int64(s.config.MaxSandboxes), MetricSandboxesRunning: uint32(len(s.records)), MetricCpuCount: uint32(s.config.MaxSandboxes * 2), MetricMemoryTotalBytes: uint64(s.config.MaxSandboxes) * 512 * 1024 * 1024}, nil
}
