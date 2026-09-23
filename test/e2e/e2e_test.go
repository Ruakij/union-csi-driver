//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	driverNS = "union-csi"
	ns       = "e2e"
	image    = "union-csi-driver"
	busybox  = "busybox:1.37"
	repoRoot = "../.."
)

var (
	cluster  = envOr("E2E_CLUSTER", "union-csi-e2e")
	kubeCtx  = "kind-" + cluster
	backends = []string{"mergerfs", "overlay"}
	client   kubernetes.Interface
	node     string
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	code := 1
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
	} else {
		code = m.Run()
	}
	if code != 0 {
		diagnose()
	}
	if os.Getenv("E2E_KEEP") == "" {
		_ = stream(exec.Command("kind", "delete", "cluster", "--name", cluster))
	}
	os.Exit(code)
}

func setup() error {
	if out, _ := run("kind", "get", "clusters"); !slices.Contains(strings.Fields(out), cluster) {
		if err := stream(exec.Command("kind", "create", "cluster", "--name", cluster, "--wait", "120s")); err != nil {
			return err
		}
	}
	if os.Getenv("E2E_SKIP_BUILD") == "" {
		if err := stream(exec.Command("docker", "build", "-t", image+":e2e", repoRoot)); err != nil {
			return err
		}
	}
	// Tagged by content, so a rebuilt image changes the pod template and rolls the driver.
	id, err := run("docker", "image", "inspect", "-f", "{{.Id}}", image+":e2e")
	if err != nil {
		return err
	}
	tag := "e2e-" + strings.TrimPrefix(id, "sha256:")[:12]
	if _, err := run("docker", "tag", image+":e2e", image+":"+tag); err != nil {
		return err
	}
	if err := stream(exec.Command("kind", "load", "docker-image", "--name", cluster, image+":"+tag)); err != nil {
		return err
	}

	for _, b := range backends {
		// kind's default StorageClass provisions hostPath PVs under this directory.
		err := stream(exec.Command("helm", "--kube-context", kubeCtx, "upgrade", "--install", b, repoRoot+"/charts/union-csi-driver",
			"-n", driverNS, "--create-namespace", "--wait", "--timeout", "3m",
			"--set", "backend="+b,
			"--set", "image.repository="+image, "--set", "image.tag="+tag, "--set", "image.pullPolicy=Never",
			"--set", "hostPaths.allowed={/var/local-path-provisioner}"))
		if err != nil {
			return err
		}
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeCtx}).ClientConfig()
	if err != nil {
		return err
	}
	if client, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	ctx := context.Background()
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	node = nodes.Items[0].Name

	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return seed(ctx)
}

// seed fills the top and bottom PVCs before any union mounts them: overlay
// leaves lower-layer edits while mounted undefined.
func seed(ctx context.Context) error {
	for _, name := range []string{"top", "bottom"} {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Mi")}},
			},
		}
		_, err := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	p := pod("seed", claim("top"), claim("bottom"))
	p.Spec.RestartPolicy = corev1.RestartPolicyNever
	p.Spec.Containers[0].Command = []string{"sh", "-c",
		"echo top > /src/top/shared; echo a > /src/top/a; echo bottom > /src/bottom/shared; echo b > /src/bottom/b"}
	if _, err := client.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
		return err
	}
	err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := client.CoreV1().Pods(ns).Get(ctx, "seed", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if p.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("seed pod failed")
		}
		return p.Status.Phase == corev1.PodSucceeded, nil
	})
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	return deletePods(ctx, "seed")
}

func TestBackends(t *testing.T) {
	for _, b := range backends {
		t.Run(b, func(t *testing.T) { testBackend(t, b) })
	}
}

func testBackend(t *testing.T, b string) {
	driver := b + ".csi.ruekov.eu"
	name := "merge-" + b
	upper := "/src/rw"
	if b == "overlay" {
		upper = "/src/rw/.union-csi/upper"
	}

	// rw is the writable top layer, top shadows bottom.
	create(t, pod(name,
		emptyDir("rw"), emptyDir("rw2"), claim("top"), claim("bottom"),
		union("merged", driver, "rw,top=RO,bottom=RO", false, ""),
		union("merged-ro", driver, "rw2,top=RO,bottom=RO", true, "")))
	t.Cleanup(func() { _ = deletePods(context.Background(), name) })
	ready(t, name)

	t.Run("merged view", func(t *testing.T) {
		expect(t, name, "cat /merged/shared", "top")
		expect(t, name, "cat /merged/a /merged/b", "a\nb")
		expect(t, name, "cat /merged-ro/shared", "top")
	})

	t.Run("writes land in the RW source only", func(t *testing.T) {
		expect(t, name, "echo new > /merged/new && cat "+upper+"/new", "new")
		expectFail(t, name, "test -e /src/top/new || test -e /src/bottom/new")
	})

	t.Run("readOnly volume refuses writes", func(t *testing.T) {
		expectFail(t, name, "echo x > /merged-ro/x")
	})

	if b == "mergerfs" {
		t.Run("branch edits appear without remounting", func(t *testing.T) {
			expect(t, name, "echo direct > /src/rw/direct && cat /merged/direct", "direct")
		})
	}

	t.Run("invalid attributes are refused", func(t *testing.T) {
		badOpt, badName := "bad-opt-"+b, "bad-name-"+b
		opt := create(t, pod(badOpt, emptyDir("rw"), union("merged", driver, "rw", false, "upperdir=/etc")))
		name := create(t, pod(badName, emptyDir("rw"), union("merged", driver, "../../../etc", false, "")))
		t.Cleanup(func() { _ = deletePods(context.Background(), badOpt, badName) })
		failedMount(t, opt, "InvalidArgument")
		failedMount(t, name, "InvalidArgument")
	})

	t.Run("mounts survive a driver restart", func(t *testing.T) {
		ds, before := daemonSet(t, b), driverPod(t, b)
		kubectl(t, "-n", driverNS, "rollout", "restart", "ds/"+ds)
		kubectl(t, "-n", driverNS, "rollout", "status", "ds/"+ds, "--timeout=120s")
		if driverPod(t, b) == before {
			t.Fatal("driver pod was not replaced")
		}
		expect(t, name, "cat /merged/shared", "top")
		expect(t, name, "echo after > /merged/after && cat "+upper+"/after", "after")
	})

	t.Run("a pod created while the driver is down mounts once it returns", func(t *testing.T) {
		ds, late := daemonSet(t, b), "late-"+b
		patchDS(t.Context(), t, ds, `{"spec":{"template":{"spec":{"nodeSelector":{"e2e/absent":"true"}}}}}`)
		t.Cleanup(func() { patchDS(context.Background(), t, ds, `{"spec":{"template":{"spec":{"nodeSelector":null}}}}`) })
		poll(t, 2*time.Minute, "driver pods gone", func(ctx context.Context) (bool, error) {
			pods, err := client.CoreV1().Pods(driverNS).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + b})
			return err == nil && len(pods.Items) == 0, err
		})
		p := create(t, pod(late, emptyDir("rw"), union("merged", driver, "rw", false, "")))
		t.Cleanup(func() { _ = deletePods(context.Background(), late) })
		failedMount(t, p, driver)
		patchDS(t.Context(), t, ds, `{"spec":{"template":{"spec":{"nodeSelector":null}}}}`)
		ready(t, late)
	})

	t.Run("unpublish leaves nothing behind", func(t *testing.T) {
		if err := deletePods(t.Context(), name, "late-"+b); err != nil {
			t.Fatal(err)
		}
		// The pod object goes away before kubelet has finished unpublishing.
		poll(t, time.Minute, "no union mounts or mergerfs daemons on the node", func(context.Context) (bool, error) {
			out, err := run("docker", "exec", node, "sh", "-c",
				"echo $(($(grep -c kubernetes.io~csi /proc/self/mountinfo) + $(pgrep -c mergerfs)))")
			return out == "0", err
		})
	})
}

func pod(name string, vols ...corev1.Volume) *corev1.Pod {
	c := corev1.Container{Name: "app", Image: busybox, Command: []string{"sleep", "infinity"}}
	for _, v := range vols {
		path := "/src/" + v.Name
		if v.CSI != nil {
			path = "/" + v.Name
		}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: v.Name, MountPath: path})
	}
	zero := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &zero,
			Containers:                    []corev1.Container{c},
			Volumes:                       vols,
		},
	}
}

func emptyDir(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func claim(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}
}

func union(name, driver, sources string, readOnly bool, options string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver:           driver,
		ReadOnly:         &readOnly,
		VolumeAttributes: map[string]string{"sourceVolumes": sources, "options": options},
	}}}
}

func create(t *testing.T, p *corev1.Pod) *corev1.Pod {
	t.Helper()
	p, err := client.CoreV1().Pods(ns).Create(t.Context(), p, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func deletePods(ctx context.Context, names ...string) error {
	for _, n := range names {
		err := client.CoreV1().Pods(ns).Delete(ctx, n, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		for _, n := range names {
			if _, err := client.CoreV1().Pods(ns).Get(ctx, n, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return false, nil
			}
		}
		return true, nil
	})
}

func ready(t *testing.T, name string) {
	t.Helper()
	poll(t, 3*time.Minute, name+" ready", func(ctx context.Context) (bool, error) {
		p, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// failedMount keys on the pod UID: events outlive a deleted pod of the same name.
func failedMount(t *testing.T, p *corev1.Pod, substr string) {
	t.Helper()
	poll(t, 2*time.Minute, fmt.Sprintf("FailedMount event on %s containing %q", p.Name, substr), func(ctx context.Context) (bool, error) {
		evs, err := client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.uid=" + string(p.UID) + ",reason=FailedMount"})
		if err != nil {
			return false, err
		}
		for _, e := range evs.Items {
			if strings.Contains(e.Message, substr) {
				return true, nil
			}
		}
		return false, nil
	})
}

func daemonSet(t *testing.T, release string) string {
	t.Helper()
	list, err := client.AppsV1().DaemonSets(driverNS).List(t.Context(),
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + release})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("daemonset of release %s: %v", release, err)
	}
	return list.Items[0].Name
}

func driverPod(t *testing.T, release string) types.UID {
	t.Helper()
	pods, err := client.CoreV1().Pods(driverNS).List(t.Context(),
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + release})
	if err != nil {
		t.Fatal(err)
	}
	// Rollout status is done once the new pod is ready, while the old one may still be terminating.
	pods.Items = slices.DeleteFunc(pods.Items, func(p corev1.Pod) bool { return p.DeletionTimestamp != nil })
	if len(pods.Items) != 1 {
		t.Fatalf("release %s has %d running driver pods, want 1", release, len(pods.Items))
	}
	return pods.Items[0].UID
}

func patchDS(ctx context.Context, t *testing.T, name, patch string) {
	t.Helper()
	_, err := client.AppsV1().DaemonSets(driverNS).Patch(ctx, name,
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func poll(t *testing.T, timeout time.Duration, what string, cond wait.ConditionWithContextFunc) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, timeout, true, cond); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}

func exe(podName, cmd string) (string, error) {
	return run("kubectl", "--context", kubeCtx, "-n", ns, "exec", podName, "-c", "app", "--", "sh", "-c", cmd)
}

func expect(t *testing.T, podName, cmd, want string) {
	t.Helper()
	got, err := exe(podName, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: %q printed %q, want %q", podName, cmd, got, want)
	}
}

func expectFail(t *testing.T, podName, cmd string) {
	t.Helper()
	if _, err := exe(podName, cmd); err == nil {
		t.Fatalf("%s: %q succeeded, want failure", podName, cmd)
	}
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	if _, err := run("kubectl", append([]string{"--context", kubeCtx}, args...)...); err != nil {
		t.Fatal(err)
	}
}

// run returns trimmed stdout; the error carries stderr.
func run(name string, args ...string) (string, error) {
	var stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// stream runs a long setup step with its output visible.
func stream(cmd *exec.Cmd) error {
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func diagnose() {
	for _, args := range [][]string{
		{"-n", ns, "get", "pods", "-o", "wide"},
		{"-n", ns, "get", "events", "--sort-by=.lastTimestamp"},
		{"-n", driverNS, "logs", "-l", "app.kubernetes.io/name=union-csi-driver", "-c", "union-csi-driver", "--tail=80", "--prefix"},
	} {
		_ = stream(exec.Command("kubectl", append([]string{"--context", kubeCtx}, args...)...))
	}
}
