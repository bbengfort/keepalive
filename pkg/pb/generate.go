package pb

//go:generate protoc -I=../../proto --go_out=. --go_opt=module=github.com/bbengfort/keepalive/pkg/pb --go-grpc_out=. --go-grpc_opt=module=github.com/bbengfort/keepalive/pkg/pb v1/keepalive.proto
