package compute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/runtime-bridge/backend"
)

func TestComputeLifecycleContract(t *testing.T) {
	t.Parallel()
	state := "creating"
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Query().Get("branch") != "isolated" {
			t.Error("missing tenant authentication or branch scope")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenant/services":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["port"] != float64(0) || body["always_on"] != true || body["cpu_milli"] != float64(2000) || body["memory_mib"] != float64(512) || body["branch"] != "isolated" {
				t.Errorf("incorrect create mapping: %v", body)
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenant/services/e2b-sandbox01":
			if state == "deleted" {
				w.WriteHeader(http.StatusNotFound)

				return
			}
			gets++
			if gets >= 2 {
				state = "running"
			}
			_ = json.NewEncoder(w).Encode(backend.Instance{Name: "e2b-sandbox01", State: state})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			var body struct {
				Cmd     []string `json:"cmd"`
				Timeout int      `json:"timeout_s"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body.Cmd, []string{"printf", "hello"}) || body.Timeout != 60 {
				t.Errorf("wrong exec mapping: %v", body)
			}
			_ = json.NewEncoder(w).Encode(backend.Result{Stdout: "hello", Stderr: "bad", ExitCode: 7})
		case r.Method == http.MethodDelete:
			state = "deleted"
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, Token: "secret", Tenant: "tenant", Branch: "isolated"})
	if err != nil {
		t.Fatal(err)
	}
	c.poll = time.Millisecond
	if err := c.Create(t.Context(), backend.Spec{Name: "e2b-sandbox01", Image: "image", CPU: 2, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	if gets < 2 {
		t.Fatal("create returned before the runtime reached running")
	}
	result, err := c.Exec(t.Context(), "e2b-sandbox01", []string{"printf", "hello"})
	if err != nil || result.ExitCode != 7 || result.Stdout != "hello" || result.Stderr != "bad" {
		t.Fatalf("incorrect command result: %v %v", result, err)
	}
	if err := c.Delete(t.Context(), "e2b-sandbox01"); err != nil {
		t.Fatal(err)
	}
}

func TestComputeFailureAndCancellation(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"failed", "creating"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)

					return
				}
				_ = json.NewEncoder(w).Encode(backend.Instance{Name: "sandbox", State: state})
			}))
			defer server.Close()
			c, _ := New(Config{URL: server.URL, Token: "secret", Tenant: "tenant", Branch: "poc"})
			c.poll = time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := c.Create(ctx, backend.Spec{Name: "sandbox", Image: "image", CPU: 1, MemoryMiB: 512})
			if err == nil {
				t.Fatal("unready backend was reported as ready")
			}
			if state == "creating" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestComputeErrorsDoNotExposeBackendResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("secret-environment-value"))
	}))
	defer server.Close()
	c, _ := New(Config{URL: server.URL, Token: "secret", Tenant: "tenant", Branch: "poc"})
	_, err := c.Get(t.Context(), "sandbox")
	if err == nil || strings.Contains(err.Error(), "secret-environment-value") {
		t.Fatal(err)
	}
}

func TestMissingExitStatusIsNotSuccess(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stdout":"123\nhello"}`))
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, Token: "secret", Tenant: "tenant", Branch: "poc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(t.Context(), "sandbox", []string{"true"}); err == nil {
		t.Fatal("missing exit_code was accepted as a successful command")
	}
}
