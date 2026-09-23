package driver

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func TestSanity(t *testing.T) {
	// Short, since unix socket paths are capped near 100 bytes.
	dir, err := os.MkdirTemp("", "csi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "csi.sock")

	be := overlayLikeBackend()
	d, err := New(Config{
		VendorVersion:    "sanity",
		DriverName:       "sanity.csi.ruekov.eu",
		NodeID:           "node",
		Endpoint:         "unix://" + sock,
		MaxSourceVolumes: 32,
		Backend:          be,
		Policy:           backend.NewPolicy(be.Schema(), backend.PolicyConfig{DenylistMode: backend.DenylistRefuse}),
		KubeClient:       fake.NewClientset(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	done := make(chan error)
	go func() { done <- d.Run(stop) }()
	t.Cleanup(func() { stop <- syscall.SIGTERM; <-done })

	cfg := sanity.NewTestConfig()
	cfg.Address = sock
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	sc := sanity.GinkgoTest(&cfg)
	gomega.RegisterFailHandler(ginkgo.Fail)

	suite, reporter := ginkgo.GinkgoConfiguration()
	// There is no controller, and publishing needs a volume from CreateVolume.
	suite.SkipStrings = append(suite.SkipStrings, `\[Controller Server\]`, "should remove target path")
	ginkgo.RunSpecs(t, "CSI sanity", suite, reporter)
	sc.Finalize()
}
