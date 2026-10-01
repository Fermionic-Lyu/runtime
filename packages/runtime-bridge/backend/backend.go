package backend

import (
	"context"
	"errors"
)

var (
	ErrNotFound = errors.New("runtime instance not found")
	ErrConflict = errors.New("runtime instance already exists")
)

type Spec struct {
	Name      string
	Image     string
	Env       map[string]string
	CPU       int64
	MemoryMiB int64
}

type Instance struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int32  `json:"exit_code"`
}

type Runtime interface {
	Create(ctx context.Context, spec Spec) error
	Get(ctx context.Context, name string) (Instance, error)
	Delete(ctx context.Context, name string) error
	Exec(ctx context.Context, name string, cmd []string) (Result, error)
}
