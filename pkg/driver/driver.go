// Package driver implements the backend-agnostic CSI Identity and Node services.
// It parses volumeAttributes, resolves sibling pod volumes via pkg/volsource, and
// delegates the actual mount to the configured pkg/backend.Backend. Staging is not
// advertised: this driver is ephemeral-inline-volumes only.
package driver

import (
	"errors"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog/v2"
)

// Driver implements csi.IdentityServer and csi.NodeServer.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	config Config
}

// New validates cfg and constructs a Driver.
func New(cfg Config) (*Driver, error) {
	if cfg.DriverName == "" {
		return nil, errors.New("no driver name provided")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("no node id provided")
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("no driver endpoint provided")
	}
	if cfg.Backend == nil {
		return nil, errors.New("no backend configured")
	}
	if cfg.Policy == nil {
		return nil, errors.New("no option policy configured")
	}

	klog.Infof("Driver: %v", cfg.DriverName)
	klog.Infof("Version: %s", cfg.VendorVersion)
	klog.Infof("Backend: %s", cfg.Backend.Name())

	return &Driver{config: cfg}, nil
}

// Run starts the gRPC server and blocks until stopCh fires.
func (d *Driver) Run(stopCh <-chan os.Signal) error {
	s := newNonBlockingGRPCServer()
	s.Start(d.config.Endpoint, d, d)
	<-stopCh
	s.Stop()
	return nil
}
