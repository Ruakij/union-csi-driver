package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/Ruakij/union-csi-driver/internal/proxy"
	"github.com/Ruakij/union-csi-driver/pkg/backend"
	_ "github.com/Ruakij/union-csi-driver/pkg/backend/mergerfs"
	_ "github.com/Ruakij/union-csi-driver/pkg/backend/overlay"
	"github.com/Ruakij/union-csi-driver/pkg/driver"
)

var (
	// Set by the build process
	version = ""
)

// csvFlag is a flag.Value collecting a comma-separated list into a []string.
type csvFlag struct {
	values *[]string
}

func (f csvFlag) String() string {
	if f.values == nil {
		return ""
	}
	return strings.Join(*f.values, ",")
}

func (f csvFlag) Set(value string) error {
	if value == "" {
		*f.values = nil
		return nil
	}
	*f.values = strings.Split(value, ",")
	return nil
}

// kvFlag is a flag.Value parsing a comma-separated key=value list into a map.
type kvFlag struct {
	values *map[string]string
}

func (f kvFlag) String() string {
	if f.values == nil {
		return ""
	}
	parts := make([]string, 0, len(*f.values))
	for k, v := range *f.values {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (f kvFlag) Set(value string) error {
	result := map[string]string{}
	if value != "" {
		for _, pair := range strings.Split(value, ",") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				result[kv[0]] = kv[1]
			} else {
				result[kv[0]] = ""
			}
		}
	}
	*f.values = result
	return nil
}

func main() {
	var cfg driver.Config
	var policyCfg backend.PolicyConfig
	var backendName string
	var denylistMode string

	flag.StringVar(&cfg.Endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&cfg.DriverName, "drivername", "", "name of the driver (default: \"<backend>.csi.ruekov.eu\")")
	flag.StringVar(&cfg.NodeID, "nodeid", "", "node id")
	flag.StringVar(&backendName, "backend", "", fmt.Sprintf("merge backend to run (%s)", strings.Join(backend.Names(), ", ")))
	flag.StringVar(&cfg.KubeletRoot, "kubelet-root", "/var/lib/kubelet", "path to the kubelet directory on this node")
	flag.StringVar(&cfg.StateDir, "state-dir", "", "where the backend keeps per-volume node state (default: \"<kubelet-root>/plugins/<drivername>/state\")")
	flag.DurationVar(&cfg.PublishTimeout, "publish-timeout", 30*time.Second, "how long NodePublishVolume waits for sibling volumes to become ready")
	flag.IntVar(&cfg.MaxSourceVolumes, "max-source-volumes", 32, "maximum number of sourceVolumes entries accepted per volume")

	flag.Var(csvFlag{&policyCfg.Allowlist}, "option-allowlist", "comma-separated list of allowed backend options (empty: any schema-known option not denied)")
	flag.Var(csvFlag{&policyCfg.Denylist}, "option-denylist", "comma-separated list of denied backend options (empty: backend default)")
	flag.StringVar(&denylistMode, "denylist-mode", "refuse", "how to handle a denied pod option: refuse | strip")
	flag.Var(kvFlag{&policyCfg.Defaults}, "default-options", "comma-separated key=value backend options applied when the pod does not set them")
	flag.Var(kvFlag{&policyCfg.Forced}, "forced-options", "comma-separated key=value backend options applied last, not pod-overridable")

	showVersion := flag.Bool("version", false, "show version")
	// The proxy-endpoint option is intended to be used by the Kubernetes E2E test
	// suite for proxying incoming calls to the embedded mock CSI driver.
	proxyEndpoint := flag.String("proxy-endpoint", "", "instead of running the CSI driver code, just proxy connections from csiEndpoint to the given listening socket")

	klog.InitFlags(nil)
	flag.Parse()

	if *showVersion {
		baseName := path.Base(os.Args[0])
		fmt.Println(baseName, version)
		return
	}

	cfg.VendorVersion = version
	policyCfg.DenylistMode = backend.DenylistMode(denylistMode)
	if policyCfg.DenylistMode != backend.DenylistRefuse && policyCfg.DenylistMode != backend.DenylistStrip {
		klog.Fatalf("invalid --denylist-mode %q, must be refuse or strip", denylistMode)
	}

	if *proxyEndpoint != "" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		closer, err := proxy.Run(ctx, cfg.Endpoint, *proxyEndpoint)
		if err != nil {
			klog.Fatalf("failed to run proxy: %v", err)
		}
		defer func() { _ = closer.Close() }()

		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
		<-sigc
		return
	}

	be, ok := backend.Get(backendName)
	if !ok {
		klog.Fatalf("unknown --backend %q, must be one of: %s", backendName, strings.Join(backend.Names(), ", "))
	}
	cfg.Backend = be

	if cfg.DriverName == "" {
		cfg.DriverName = backendName + ".csi.ruekov.eu"
	}
	if cfg.StateDir == "" {
		// Alongside the plugin socket, which is already a hostPath the DaemonSet
		// mounts, so state outlives the pod without another volume.
		cfg.StateDir = filepath.Join(cfg.KubeletRoot, "plugins", cfg.DriverName, "state")
	}

	// Fall back to the backend's own defaults only for flags the admin left alone,
	// so an explicitly empty list stays empty.
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if !setFlags["default-options"] {
		policyCfg.Defaults = be.DefaultOptions()
	}
	if !setFlags["option-denylist"] {
		policyCfg.Denylist = be.DefaultDenylist()
	}

	policy := backend.NewPolicy(be.Schema(), policyCfg)
	if err := policy.Validate(); err != nil {
		klog.Fatalf("invalid option policy: %v", err)
	}
	cfg.Policy = policy

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("failed to build in-cluster kubeconfig: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("failed to build kubernetes client: %v", err)
	}
	cfg.KubeClient = kubeClient

	d, err := driver.New(cfg)
	if err != nil {
		klog.Fatalf("failed to initialize driver: %v", err)
	}

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)

	if err := d.Run(stopCh); err != nil {
		klog.Fatalf("driver exited: %v", err)
	}
}
