module github.com/e2b-dev/infra/packages/runtime-bridge

go 1.26.8

replace github.com/e2b-dev/infra/packages/shared => ../shared

require (
	connectrpc.com/connect v1.18.1
	github.com/e2b-dev/infra/packages/shared v0.0.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260825221802-da73d73af1c5 // indirect
)
