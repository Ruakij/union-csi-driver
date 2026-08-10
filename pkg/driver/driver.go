// Package driver implements the backend-agnostic CSI Identity and Node services.
// It parses volumeAttributes, resolves sibling pod volumes via pkg/volsource, and
// delegates the actual mount to the configured pkg/backend.Backend. Staging is not
// advertised: this driver is ephemeral-inline-volumes only.
package driver

import (
	"context"
	"errors"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
	"github.com/Ruakij/union-csi-driver/pkg/volsource"
)

// Driver implements csi.IdentityServer and csi.NodeServer.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	config   Config
	resolver *volsource.Resolver
	mounter  mount.Interface
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
	if cfg.KubeClient == nil {
		return nil, errors.New("no kubernetes client configured")
	}

	klog.Infof("Driver: %v", cfg.DriverName)
	klog.Infof("Version: %s", cfg.VendorVersion)
	klog.Infof("Backend: %s", cfg.Backend.Name())

	if runner, ok := cfg.Backend.(backend.Runner); ok {
		if err := runner.Init(cfg.StateDir); err != nil {
			return nil, err
		}
	}

	return &Driver{
		config:   cfg,
		resolver: volsource.NewResolver(cfg.KubeClient, cfg.KubeletRoot, cfg.DriverName),
		mounter:  mount.New(""),
	}, nil
}

// Run starts the gRPC server and the backend's maintenance loop, if it has one,
// and blocks until stopCh fires.
func (d *Driver) Run(stopCh <-chan os.Signal) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if runner, ok := d.config.Backend.(backend.Runner); ok {
		go runner.Run(ctx)
	}

	s := newNonBlockingGRPCServer()
	s.Start(d.config.Endpoint, d, d)
	<-stopCh
	s.Stop()
	return nil
}
