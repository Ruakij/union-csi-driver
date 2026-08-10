package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	attrEphemeral = "csi.storage.k8s.io/ephemeral"
)

// NodePublishVolume resolves sibling pod volumes and mounts the union at
// req.TargetPath.
//
// TODO wire up once pkg/backend/policy attribute parsing (plan section 1),
// pkg/volsource.Resolve/WaitReady (sections 2-3) and a real backend are in place:
// parse sourceVolumes/options, resolve via volsource, poll readiness, check
// mountinfo idempotency, os.MkdirAll(TargetPath), Backend.Mount.
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path missing in request")
	}
	if req.GetVolumeContext()[attrEphemeral] != "true" {
		return nil, status.Error(codes.InvalidArgument, "this driver only supports ephemeral inline volumes")
	}

	return nil, status.Error(codes.Unimplemented, "NodePublishVolume: mount logic not implemented yet")
}

// NodeUnpublishVolume unmounts req.TargetPath and removes it.
//
// TODO wire up once a real backend exists: Backend.Unmount (tolerate not-mounted),
// remove the directory, succeed if already gone.
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path missing in request")
	}

	return nil, status.Error(codes.Unimplemented, "NodeUnpublishVolume: unmount logic not implemented yet")
}

func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.config.NodeID,
	}, nil
}

// NodeGetCapabilities returns no capabilities: staging is not advertised (sources
// are pod-scoped, keyed by pod UID, and cannot be computed once and shared).
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
