package grpc

import (
	"context"
	"net"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/futureq-io/futureq/internal/api/grpc/handlers"
	"github.com/futureq-io/futureq/internal/app"
	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/dispatcher"
	pb "github.com/futureq-io/protocol/proto/go"
)

// Server wraps a *grpc.Server and exposes lifecycle methods.
type Server struct {
	srv    *grpc.Server
	logger *zap.Logger
	addr   string
}

// New creates a fully configured gRPC server and registers all service
// handlers. No network socket is opened yet; call Listen to do that.
func New(
	cfg config.Server,
	hub *dispatcher.Hub,
	deleter *dispatcher.Deleter,
	logger *zap.Logger,
) *Server {
	log := logger.Named("grpc_server")

	srv := grpc.NewServer(
		grpc.MaxConcurrentStreams(cfg.MaxConns),
		grpc.MaxRecvMsgSize(cfg.MaxRecvSizeKB*1024), // KB
		grpc.MaxSendMsgSize(cfg.MaxSendSizeKB*1024), // KB

		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),

		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     30 * time.Second,
			MaxConnectionAge:      2 * time.Minute,
			MaxConnectionAgeGrace: 10 * time.Second,
			Time:                  10 * time.Second,
			Timeout:               cfg.Timeout,
		}),
	)

	// Register all service implementations.
	pb.RegisterFutureQProducerServer(srv, handlers.NewProducerHandler(log))
	pb.RegisterFutureQConsumerServer(srv, handlers.NewConsumerHandler(log, hub, deleter))

	return &Server{
		srv:    srv,
		logger: log,
		addr:   cfg.Listen,
	}
}

// Listen binds the TCP listener and blocks serving until the underlying
// gRPC server is stopped. Call Shutdown to stop it gracefully.
func (s *Server) Listen() *Server {
	go func() {
		lis, err := net.Listen("tcp", s.addr)
		if err != nil {
			s.logger.Fatal("gRPC: failed to bind", zap.String("address", s.addr), zap.Error(err))
		}

		s.logger.Info("gRPC server listening", zap.String("address", s.addr))

		if err := s.srv.Serve(lis); err != nil {
			s.logger.Fatal("gRPC: failed to serve", zap.Error(err))
		}
	}()

	return s
}

// WaitForShutdown registers a background shutdown handler that runs when ctx
// (the global app.Ctx) is cancelled. It gracefully stops the gRPC server
// within the deadline carried by app.A.ShutCtx (or a 10s fallback).
func (s *Server) WaitForShutdown(ctx context.Context) {
	app.A.RegisterComponentWithShutdown()

	go func() {
		defer app.A.ComponentShutdownDone()

		<-ctx.Done()

		s.logger.Info("gRPC server: initiating graceful shutdown")

		shutCtx := app.A.ShutCtx

		done := make(chan struct{})
		go func() {
			s.srv.GracefulStop()
			close(done)
		}()

		select {
		case <-done:
			s.logger.Info("gRPC server: stopped gracefully")
		case <-shutCtx.Done():
			s.logger.Warn("gRPC server: graceful shutdown timed out, forcing stop")
			s.srv.Stop()
		}
	}()
}
