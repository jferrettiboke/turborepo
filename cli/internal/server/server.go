package server

import (
	context "context"

	"google.golang.org/grpc"
)

type Server struct {
	UnimplementedTurboServer
}

func New() *Server {
	return &Server{}
}

func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	RegisterTurboServer(registrar, s)
}

func (s *Server) GetGlobalHash(ctx context.Context, req *GlobalHashRequest) (*GlobalHashReply, error) {
	hash := "foo"
	return &GlobalHashReply{Hash: []byte(hash)}, nil
}

func (s *Server) Ping(ctx context.Context, req *PingRequest) (*PingReply, error) {
	return &PingReply{}, nil
}
