package bridge

import (
	"context"
	"crypto/subtle"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
)

type processServer struct {
	processconnect.UnimplementedProcessHandler

	bridge *Server
	id     string
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)

		return
	}
	id := r.Header.Get("E2b-Sandbox-Id")
	s.mu.Lock()
	rec, ok := s.records[id]
	valid := ok && rec.Phase == "running" && rec.ExpiresAt.After(time.Now()) && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Access-Token")), []byte(rec.Request.GetSandbox().GetEnvdAccessToken())) == 1
	s.mu.Unlock()
	if !valid {
		http.Error(w, "invalid sandbox or access token", http.StatusUnauthorized)

		return
	}
	if port := r.Header.Get("E2b-Sandbox-Port"); port != "" && port != "49983" {
		http.Error(w, "only the process API is supported", http.StatusNotImplemented)

		return
	}
	username, _, hasUser := r.BasicAuth()
	if !hasUser || username != "root" {
		http.Error(w, "POC commands require user=root", http.StatusNotImplemented)

		return
	}
	_, handler := processconnect.NewProcessHandler(&processServer{bridge: s, id: id})
	handler.ServeHTTP(w, r)
}

func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }

func (p *processServer) Start(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	if req.Msg.GetPty() != nil || req.Msg.GetStdin() || req.Msg.Tag != nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("PTY, stdin and tagged/background processes are unsupported"))
	}
	cfg := req.Msg.GetProcess()
	if cfg == nil || cfg.GetCmd() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("command is required"))
	}
	p.bridge.mu.Lock()
	rec, ok := p.bridge.records[p.id]
	if !ok || rec.Phase != "running" {
		p.bridge.mu.Unlock()

		return connect.NewError(connect.CodeNotFound, errors.New("sandbox is not running"))
	}
	name := rec.Name
	envs := map[string]string{}
	maps.Copy(envs, rec.Request.GetSandbox().GetEnvVars())
	maps.Copy(envs, cfg.GetEnvs())
	p.bridge.mu.Unlock()
	ctx, cancelExec := context.WithTimeout(ctx, 60*time.Second)
	defer cancelExec()
	args := []string{"env"}
	keys := make([]string, 0, len(envs))
	for k := range envs {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid environment key"))
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, k+"="+envs[k])
	}
	var command strings.Builder
	command.WriteString(shellQuote(cfg.GetCmd()))
	for _, arg := range cfg.GetArgs() {
		command.WriteString(" " + shellQuote(arg))
	}
	cwd := "/"
	if cfg.Cwd != nil {
		cwd = cfg.GetCwd()
	}
	script := "printf '%s\\n' \"$$\"; cd " + shellQuote(cwd) + " || exit 125; exec " + command.String()
	args = append(args, "sh", "-c", script)
	result, err := p.bridge.runtime.Exec(ctx, name, args)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	pidText, stdout, found := strings.Cut(result.Stdout, "\n")
	pid, err := strconv.ParseUint(pidText, 10, 32)
	if !found || err != nil || pid == 0 {
		return connect.NewError(connect.CodeInternal, errors.New("runtime did not return the execution PID"))
	}
	events := []*process.ProcessEvent{{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: uint32(pid)}}}}
	if stdout != "" {
		events = append(events, &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(stdout)}}}})
	}
	if result.Stderr != "" {
		events = append(events, &process.ProcessEvent{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte(result.Stderr)}}}})
	}
	events = append(events, &process.ProcessEvent{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{Exited: true, ExitCode: result.ExitCode, Status: "exited"}}})
	for _, event := range events {
		if err := stream.Send(&process.StartResponse{Event: event}); err != nil {
			return err
		}
	}

	return nil
}
