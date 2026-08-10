package driver

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"github.com/Ruakij/union-csi-driver/internal/endpoint"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
)

// nonBlockingGRPCServer serves the Identity and Node services on a goroutine.
type nonBlockingGRPCServer struct {
	server  *grpc.Server
	cleanup func()
}

func newNonBlockingGRPCServer() *nonBlockingGRPCServer {
	return &nonBlockingGRPCServer{}
}

func (s *nonBlockingGRPCServer) Start(ep string, ids csi.IdentityServer, ns csi.NodeServer) {
	go s.serve(ep, ids, ns)
}

func (s *nonBlockingGRPCServer) Stop() {
	s.server.GracefulStop()
	s.cleanup()
}

func (s *nonBlockingGRPCServer) ForceStop() {
	s.server.Stop()
	s.cleanup()
}

func (s *nonBlockingGRPCServer) serve(ep string, ids csi.IdentityServer, ns csi.NodeServer) {
	listener, cleanup, err := endpoint.Listen(ep)
	if err != nil {
		klog.Fatalf("Failed to listen: %v", err)
	}

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(logGRPC),
	}
	server := grpc.NewServer(opts...)
	s.server = server
	s.cleanup = cleanup

	csi.RegisterIdentityServer(server, ids)
	csi.RegisterNodeServer(server, ns)

	klog.Infof("Listening for connections on address: %#v", listener.Addr())

	server.Serve(listener) //nolint:errcheck
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	pri := klog.Level(3)
	if info.FullMethod == "/csi.v1.Identity/Probe" {
		pri = 5
	}
	klog.V(pri).Infof("GRPC call: %s", info.FullMethod)

	v5 := klog.V(5)
	if v5.Enabled() {
		v5.Infof("GRPC request: %s", protosanitizer.StripSecrets(req))
	}
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("GRPC error: %v", err)
	}
	if v5.Enabled() {
		v5.Infof("GRPC response: %s", protosanitizer.StripSecrets(resp))
		logGRPCJSON(info.FullMethod, req, resp, err)
	}

	return resp, err
}

func logGRPCJSON(method string, request, reply interface{}, err error) {
	logMessage := struct {
		Method    string
		Request   interface{}
		Response  interface{}
		Error     string
		FullError error
	}{
		Method:    method,
		Request:   request,
		Response:  reply,
		FullError: err,
	}
	if err != nil {
		logMessage.Error = err.Error()
	}

	msg, err := json.Marshal(logMessage)
	if err != nil {
		logMessage.Error = err.Error()
	}
	klog.V(5).Infof("gRPCCall: %s\n", msg)
}
