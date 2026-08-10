package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

func (d *Driver) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if d.config.DriverName == "" {
		return nil, status.Error(codes.Unavailable, "driver name not configured")
	}
	if d.config.VendorVersion == "" {
		return nil, status.Error(codes.Unavailable, "driver is missing version")
	}
	return &csi.GetPluginInfoResponse{
		Name:          d.config.DriverName,
		VendorVersion: d.config.VendorVersion,
	}, nil
}

func (d *Driver) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// GetPluginCapabilities returns no service capabilities: there is no controller
// plugin and topology is not advertised (staging is not supported, sources are
// pod-scoped and cannot be computed once and shared across nodes).
func (d *Driver) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	klog.V(5).Info("GetPluginCapabilities: no capabilities advertised (node-only, ephemeral-inline driver)")
	return &csi.GetPluginCapabilitiesResponse{}, nil
}
