package driver

import (
	"time"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
	"github.com/Ruakij/union-csi-driver/pkg/volsource"
	"k8s.io/client-go/kubernetes"
)

// Config is the driver's startup configuration, built from CLI flags.
type Config struct {
	VendorVersion string
	DriverName    string
	NodeID        string
	Endpoint      string

	KubeletRoot      string
	HostPaths        volsource.HostPaths
	StateDir         string
	PublishTimeout   time.Duration
	MaxSourceVolumes int

	Backend    backend.Backend
	Policy     *backend.Policy
	KubeClient kubernetes.Interface
}
