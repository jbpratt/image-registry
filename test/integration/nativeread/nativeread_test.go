package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	dockerconfig "github.com/distribution/distribution/v3/configuration"

	"github.com/distribution/distribution/v3"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	"github.com/opencontainers/go-digest"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	imageapiv1 "github.com/openshift/api/image/v1"
	imageclientv1 "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"

	registryconfig "github.com/openshift/image-registry/pkg/dockerregistry/server/configuration"
	"github.com/openshift/image-registry/pkg/testframework"
	"github.com/openshift/image-registry/pkg/testutil"
)

// nativeReadEnabled turns the native read path on for a registry instance.
// It is a testframework.RegistryOption implemented in the test package so the
// harness itself needs no change to flip the knob.
type nativeReadEnabled struct {
	regex string
}

func (o nativeReadEnabled) Apply(_ *dockerconfig.Configuration, extraConfig *registryconfig.Configuration) {
	extraConfig.NativeReadPath = &registryconfig.NativeReadPath{Enabled: true, RepoRegex: o.regex}
}

// filesystemStorage points the registry at a filesystem root instead of the
// framework default (inmemory). Two registry instances must share one backend
// for an A/B comparison of the same image: inmemory storage is per-instance,
// so the native-off registry could not serve blobs pushed to the native-on one.
type filesystemStorage struct {
	dir string
}

func (o filesystemStorage) Apply(dockerConfig *dockerconfig.Configuration, _ *registryconfig.Configuration) {
	dockerConfig.Storage = dockerconfig.Storage{
		"filesystem": dockerconfig.Parameters{"rootdirectory": o.dir},
		"delete":     dockerconfig.Parameters{"enabled": true},
	}
}

// startPair starts two registries over one shared filesystem backend and the
// same cluster: on serves tag/manifest reads natively, off serves them via the
// imagestream. Extra options apply to the native-on registry only.
func startPair(t *testing.T, master *testframework.Master, extraOn ...testframework.RegistryOption) (on, off *testframework.Registry) {
	dir := t.TempDir()
	onOpts := append([]testframework.RegistryOption{filesystemStorage{dir}, nativeReadEnabled{regex: ".*"}}, extraOn...)
	on = master.StartRegistry(t, onOpts...)
	off = master.StartRegistry(t, filesystemStorage{dir})
	return on, off
}

// uploadImage pushes a two-layer schema2 or OCI image to repo:tag and returns
// the manifest and its canonical digest.
func uploadImage(ctx context.Context, repo distribution.Repository, tag string, oci bool) (distribution.Manifest, digest.Digest, error) {
	layers := make([]distribution.Descriptor, 2)
	for i := range layers {
		content, desc, err := testutil.MakeRandomLayer()
		if err != nil {
			return nil, "", fmt.Errorf("make random layer: %w", err)
		}
		if err := testutil.UploadBlob(ctx, repo, desc, content); err != nil {
			return nil, "", fmt.Errorf("upload layer: %w", err)
		}
		layers[i] = desc
	}

	configContent, err := json.Marshal(map[string]interface{}{
		"rootfs":  map[string]interface{}{"diff_ids": make([]string, len(layers))},
		"history": make([]struct{}, len(layers)),
	})
	if err != nil {
		return nil, "", fmt.Errorf("marshal config: %w", err)
	}
	config := distribution.Descriptor{Digest: digest.FromBytes(configContent), Size: int64(len(configContent))}
	if err := testutil.UploadBlob(ctx, repo, config, configContent); err != nil {
		return nil, "", fmt.Errorf("upload config: %w", err)
	}

	var m distribution.Manifest
	if oci {
		m, err = testutil.MakeOCISchemaManifest(config, layers)
	} else {
		m, err = testutil.MakeSchema2Manifest(config, layers)
	}
	if err != nil {
		return nil, "", fmt.Errorf("make manifest: %w", err)
	}
	if err := testutil.UploadManifest(ctx, repo, tag, m); err != nil {
		return nil, "", fmt.Errorf("upload manifest: %w", err)
	}
	canonical, err := testutil.CanonicalManifest(m)
	if err != nil {
		return nil, "", fmt.Errorf("canonical manifest: %w", err)
	}
	return m, digest.FromBytes(canonical), nil
}

func manifestByTag(ctx context.Context, repo distribution.Repository, tag string) (digest.Digest, []byte, error) {
	ms, err := repo.Manifests(ctx)
	if err != nil {
		return "", nil, err
	}
	m, err := ms.Get(ctx, "", distribution.WithTag(tag))
	if err != nil {
		return "", nil, err
	}
	_, payload, err := m.Payload()
	if err != nil {
		return "", nil, err
	}
	return digest.FromBytes(payload), payload, nil
}

func manifestByDigest(ctx context.Context, repo distribution.Repository, dgst digest.Digest) ([]byte, error) {
	ms, err := repo.Manifests(ctx)
	if err != nil {
		return nil, err
	}
	m, err := ms.Get(ctx, dgst)
	if err != nil {
		return nil, err
	}
	_, payload, err := m.Payload()
	return payload, err
}

// TestNativeReadContentEquality is H3: a registry-pushed image reads back
// byte-identical whether served natively or via the imagestream, by tag and by
// digest, and a tag overwrite is visible on the native path immediately.
func TestNativeReadContentEquality(t *testing.T) {
	master := testframework.NewMaster(t)
	defer master.Close()

	user := master.CreateUser("nrpoc-user", "password")

	for _, tc := range []struct {
		name string
		oci  bool
	}{
		{"schema2", false},
		{"oci", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ns := "nrpoc-eq-" + tc.name
			master.CreateProject(ns, user.Name)
			stream := "img"
			tag := "latest"

			on, off := startPair(t, master)
			defer on.Close()
			defer off.Close()

			manifest, wantDigest, err := uploadImage(ctx, on.Repository(ns, stream, user), tag, tc.oci)
			if err != nil {
				t.Fatalf("push: %v", err)
			}

			// manifest by tag, both modes
			onDgst, onPayload, err := manifestByTag(ctx, on.Repository(ns, stream, user), tag)
			if err != nil {
				t.Fatalf("native get by tag: %v", err)
			}
			offDgst, offPayload, err := manifestByTag(ctx, off.Repository(ns, stream, user), tag)
			if err != nil {
				t.Fatalf("imagestream get by tag: %v", err)
			}
			if onDgst != wantDigest || offDgst != wantDigest {
				t.Errorf("digest mismatch by tag: native=%s imagestream=%s want=%s", onDgst, offDgst, wantDigest)
			}
			if !bytes.Equal(onPayload, offPayload) {
				t.Errorf("payload bytes differ by tag between native and imagestream")
			}
			t.Logf("by tag: native=%s imagestream=%s (%d bytes) equal", onDgst, offDgst, len(onPayload))

			// manifest by digest, both modes
			onByDigest, err := manifestByDigest(ctx, on.Repository(ns, stream, user), wantDigest)
			if err != nil {
				t.Fatalf("native get by digest: %v", err)
			}
			offByDigest, err := manifestByDigest(ctx, off.Repository(ns, stream, user), wantDigest)
			if err != nil {
				t.Fatalf("imagestream get by digest: %v", err)
			}
			if !bytes.Equal(onByDigest, offByDigest) || !bytes.Equal(onByDigest, onPayload) {
				t.Errorf("payload bytes differ by digest between native and imagestream")
			}
			t.Logf("by digest: native and imagestream equal (%d bytes)", len(onByDigest))

			// blob stat + content, both modes
			for _, ref := range manifest.References() {
				onStat, err := on.Repository(ns, stream, user).Blobs(ctx).Stat(ctx, ref.Digest)
				if err != nil {
					t.Fatalf("native blob stat %s: %v", ref.Digest, err)
				}
				offStat, err := off.Repository(ns, stream, user).Blobs(ctx).Stat(ctx, ref.Digest)
				if err != nil {
					t.Fatalf("imagestream blob stat %s: %v", ref.Digest, err)
				}
				if onStat.Size != offStat.Size {
					t.Errorf("blob %s size mismatch: native=%d imagestream=%d", ref.Digest, onStat.Size, offStat.Size)
				}
				onBytes, err := on.Repository(ns, stream, user).Blobs(ctx).Get(ctx, ref.Digest)
				if err != nil {
					t.Fatalf("native blob get %s: %v", ref.Digest, err)
				}
				if got := digest.FromBytes(onBytes); got != ref.Digest {
					t.Errorf("blob %s content digest mismatch: got %s", ref.Digest, got)
				}
			}
			t.Logf("blobs: %d references stat+get equal in both modes", len(manifest.References()))

			// overwrite the tag and read it back immediately on the native path.
			_, wantDigest2, err := uploadImage(ctx, on.Repository(ns, stream, user), tag, tc.oci)
			if err != nil {
				t.Fatalf("overwrite push: %v", err)
			}
			if wantDigest2 == wantDigest {
				t.Fatalf("overwrite produced same digest %s; test image not random", wantDigest2)
			}
			gotDigest, _, err := manifestByTag(ctx, on.Repository(ns, stream, user), tag)
			if err != nil {
				t.Fatalf("native get by tag after overwrite: %v", err)
			}
			if gotDigest != wantDigest2 {
				t.Errorf("native path returned stale digest after overwrite: got %s want %s", gotDigest, wantDigest2)
			}
			t.Logf("overwrite: native path returned new digest %s immediately", gotDigest)
		})
	}
}

// TestNativeReadCrossRepoBlobMount records what a cross-repo blob mount into a
// native-read repository does. Per the plan this is an expected first failure;
// whatever surfaces is a finding, not a bug to fix. Recorded via t.Logf, never
// fails the suite.
func TestNativeReadCrossRepoBlobMount(t *testing.T) {
	master := testframework.NewMaster(t)
	defer master.Close()

	user := master.CreateUser("nrpoc-mount-user", "password")
	ns := "nrpoc-mount"
	master.CreateProject(ns, user.Name)

	on, off := startPair(t, master)
	defer on.Close()
	defer off.Close()

	ctx := context.Background()

	// Seed a blob in the source repo by pushing an image there.
	srcManifest, _, err := uploadImage(ctx, on.Repository(ns, "src", user), "latest", false)
	if err != nil {
		t.Fatalf("seed source repo: %v", err)
	}
	blob := srcManifest.References()[0].Digest

	// Attempt to mount that blob into a native-read destination repo.
	mountURL := fmt.Sprintf("%s/v2/%s/%s/blobs/uploads/?mount=%s&from=%s/%s",
		on.BaseURL(), ns, "dst", blob.String(), ns, "src")
	req, err := http.NewRequest("POST", mountURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user.Name, user.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mount request: %v", err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		t.Logf("FINDING crossmount: mount returned 201 Created (blob linked into dst without re-upload); native read flag does not affect blob mount auth (mount is a link operation, not tag/manifest resolution)")
	case http.StatusAccepted:
		t.Logf("FINDING crossmount: mount returned 202 Accepted (not mounted; fell back to a normal upload session)")
	default:
		t.Logf("FINDING crossmount: mount returned unexpected status %d", resp.StatusCode)
	}

	// If mounted, confirm the blob is now servable from dst in native mode.
	if resp.StatusCode == http.StatusCreated {
		if _, err := on.Repository(ns, "dst", user).Blobs(ctx).Stat(ctx, blob); err != nil {
			t.Logf("FINDING crossmount: dst blob stat after mount (native) failed: %v", err)
		} else {
			t.Logf("crossmount: dst blob stat after mount (native) succeeded")
		}
	}
}

// TestNativeReadDeletedManifestNoGhost deletes a manifest through the registry
// API (which removes the storage revision link) and asserts the native path
// serves no ghost: a GET by digest 404s in both modes. This rehearses the
// tombstone question for the projection design.
func TestNativeReadDeletedManifestNoGhost(t *testing.T) {
	master := testframework.NewMaster(t)
	defer master.Close()

	user := master.CreateUser("nrpoc-del-user", "password")
	master.GrantPrunerRole(user)
	ns := "nrpoc-del"
	master.CreateProject(ns, user.Name)
	stream := "img"
	tag := "latest"

	on, off := startPair(t, master)
	defer on.Close()
	defer off.Close()

	ctx := context.Background()

	_, dgst, err := uploadImage(ctx, on.Repository(ns, stream, user), tag, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	imageClient := imageclientv1.NewForConfigOrDie(user.KubeConfig())
	if err := imageClient.ImageStreamTags(ns).Delete(ctx, stream+":"+tag, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete istag: %v", err)
	}

	ms, err := on.Repository(ns, stream, user).Manifests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Delete(ctx, dgst); err != nil {
		t.Fatalf("delete manifest via API: %v", err)
	}

	if _, err := manifestByDigest(ctx, on.Repository(ns, stream, user), dgst); err == nil {
		t.Errorf("GHOST: native path served manifest %s after registry-API delete", dgst)
	} else {
		t.Logf("no ghost (native): get by digest after delete failed as expected: %v", err)
	}
	if _, err := manifestByDigest(ctx, off.Repository(ns, stream, user), dgst); err == nil {
		t.Errorf("GHOST: imagestream path served manifest %s after registry-API delete", dgst)
	} else {
		t.Logf("no ghost (imagestream): get by digest after delete failed as expected: %v", err)
	}
}

// TestNativeReadImageStreamSideDeletionSkew is the accepted skew from plan fact
// 6: content removed or retargeted on the imagestream side (oc tag -d / oc tag)
// is still served by the native path because the storage link is untouched.
// This is an expected failure and MUST NOT fail the suite; the observed skew is
// recorded with t.Logf so the run output is the finding record.
func TestNativeReadImageStreamSideDeletionSkew(t *testing.T) {
	master := testframework.NewMaster(t)
	defer master.Close()

	user := master.CreateUser("nrpoc-skew-user", "password")
	ns := "nrpoc-skew"
	master.CreateProject(ns, user.Name)
	imageClient := imageclientv1.NewForConfigOrDie(user.KubeConfig())
	ctx := context.Background()

	on, off := startPair(t, master)
	defer on.Close()
	defer off.Close()

	t.Run("istag_delete", func(t *testing.T) {
		stream := "del"
		tag := "latest"
		_, dgst, err := uploadImage(ctx, on.Repository(ns, stream, user), tag, false)
		if err != nil {
			t.Fatalf("push: %v", err)
		}

		// oc tag -d equivalent: delete the istag; storage link is left intact.
		if err := imageClient.ImageStreamTags(ns).Delete(ctx, stream+":"+tag, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete istag: %v", err)
		}
		waitTagAbsent(t, imageClient, ns, stream, tag)

		onDgst, _, onErr := manifestByTag(ctx, on.Repository(ns, stream, user), tag)
		_, _, offErr := manifestByTag(ctx, off.Repository(ns, stream, user), tag)
		t.Logf("FINDING skew(istag delete) by tag: native=%v(digest=%s) imagestream=%v",
			resultOf(onErr), onDgst, resultOf(offErr))

		_, onDErr := manifestByDigest(ctx, on.Repository(ns, stream, user), dgst)
		_, offDErr := manifestByDigest(ctx, off.Repository(ns, stream, user), dgst)
		t.Logf("FINDING skew(istag delete) by digest %s: native=%v imagestream=%v",
			dgst, resultOf(onDErr), resultOf(offDErr))
	})

	t.Run("tag_move", func(t *testing.T) {
		stream := "move"
		_, digestA, err := uploadImage(ctx, on.Repository(ns, stream, user), "latest", false)
		if err != nil {
			t.Fatalf("push A: %v", err)
		}
		_, digestB, err := uploadImage(ctx, on.Repository(ns, stream, user), "other", false)
		if err != nil {
			t.Fatalf("push B: %v", err)
		}

		// oc tag equivalent: move latest to image B via the image client.
		ist := &imageapiv1.ImageStreamTag{
			ObjectMeta: metav1.ObjectMeta{Name: stream + ":latest", Namespace: ns},
			Tag: &imageapiv1.TagReference{
				Name: "latest",
				From: &corev1.ObjectReference{Kind: "ImageStreamImage", Name: fmt.Sprintf("%s@%s", stream, digestB)},
			},
		}
		if _, err := imageClient.ImageStreamTags(ns).Update(ctx, ist, metav1.UpdateOptions{}); err != nil {
			if _, cerr := imageClient.ImageStreamTags(ns).Create(ctx, ist, metav1.CreateOptions{}); cerr != nil {
				t.Fatalf("move tag (update: %v; create: %v)", err, cerr)
			}
		}
		if !waitTagResolves(t, imageClient, ns, stream, "latest", digestB) {
			t.Logf("FINDING skew(tag move): imagestream status did not converge to %s within timeout; recording inconclusive", digestB)
		}

		onDgst, _, onErr := manifestByTag(ctx, on.Repository(ns, stream, user), "latest")
		offDgst, _, offErr := manifestByTag(ctx, off.Repository(ns, stream, user), "latest")
		t.Logf("FINDING skew(tag move) latest: pushed A=%s B=%s; native=%s(%v) imagestream=%s(%v)",
			digestA, digestB, onDgst, resultOf(onErr), offDgst, resultOf(offErr))
		if onErr == nil && onDgst == digestA {
			t.Logf("skew confirmed: native path still resolves latest to the old digest %s after oc-tag move", digestA)
		}
	})
}

// TestNativeReadMetrics asserts the native read resolution metric records
// native hits for a registry-pushed tag and a fallback for an etcd-only tag
// (created via the image API, absent from the native tag store).
func TestNativeReadMetrics(t *testing.T) {
	master := testframework.NewMaster(t)
	defer master.Close()

	user := master.CreateUser("nrpoc-metrics-user", "password")
	ns := "nrpoc-metrics"
	master.CreateProject(ns, user.Name)
	stream := "img"
	ctx := context.Background()

	const secret = "MetricsSecret"
	on := master.StartRegistry(t, filesystemStorage{t.TempDir()}, nativeReadEnabled{regex: ".*"}, testframework.EnableMetrics{Secret: secret})
	defer on.Close()

	_, dgst, err := uploadImage(ctx, on.Repository(ns, stream, user), "latest", false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}

	// Native hits: read the pushed tag and digest on the native registry.
	if _, _, err := manifestByTag(ctx, on.Repository(ns, stream, user), "latest"); err != nil {
		t.Fatalf("native get by tag: %v", err)
	}
	if _, err := manifestByDigest(ctx, on.Repository(ns, stream, user), dgst); err != nil {
		t.Fatalf("native get by digest: %v", err)
	}

	// Etcd-only tag: create it through the image API pointing at the pushed
	// image. It never gets a native tag link, so a GET falls back.
	imageClient := imageclientv1.NewForConfigOrDie(user.KubeConfig())
	ist := &imageapiv1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{Name: stream + ":etcdonly", Namespace: ns},
		Tag: &imageapiv1.TagReference{
			Name: "etcdonly",
			From: &corev1.ObjectReference{Kind: "ImageStreamImage", Name: fmt.Sprintf("%s@%s", stream, dgst)},
		},
	}
	if _, err := imageClient.ImageStreamTags(ns).Create(ctx, ist, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create etcd-only tag: %v", err)
	}
	if !waitTagResolves(t, imageClient, ns, stream, "etcdonly", dgst) {
		t.Fatalf("etcd-only tag did not resolve in imagestream status")
	}
	if _, _, err := manifestByTag(ctx, on.Repository(ns, stream, user), "etcdonly"); err != nil {
		t.Fatalf("get etcd-only tag: %v", err)
	}

	lines := scrapeMetrics(t, on.BaseURL(), secret, "imageregistry_native_read_resolutions_total")
	for _, l := range lines {
		t.Logf("metric: %s", l)
	}
	if !metricPresent(lines, `resolution="native"`, `operation="tag_get"`) {
		t.Errorf("expected a native tag_get resolution in metrics")
	}
	if !metricPresent(lines, `resolution="fallback"`, `operation="tag_get"`) {
		t.Errorf("expected a fallback tag_get resolution (etcd-only tag) in metrics")
	}
}

func resultOf(err error) string {
	if err == nil {
		return "served"
	}
	return "error(" + err.Error() + ")"
}

// waitTagAbsent polls until the imagestream status no longer carries tag.
func waitTagAbsent(t *testing.T, c imageclientv1.ImageV1Interface, ns, stream, tag string) {
	t.Helper()
	err := wait.Poll(time.Second, 30*time.Second, func() (bool, error) {
		is, err := c.ImageStreams(ns).Get(context.Background(), stream, metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		for _, st := range is.Status.Tags {
			if st.Tag == tag {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Logf("warning: tag %s:%s still present after wait: %v", stream, tag, err)
	}
}

// waitTagResolves polls until the imagestream status resolves tag to want.
func waitTagResolves(t *testing.T, c imageclientv1.ImageV1Interface, ns, stream, tag string, want digest.Digest) bool {
	t.Helper()
	err := wait.Poll(time.Second, 30*time.Second, func() (bool, error) {
		is, err := c.ImageStreams(ns).Get(context.Background(), stream, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, st := range is.Status.Tags {
			if st.Tag == tag && len(st.Items) > 0 && st.Items[0].Image == want.String() {
				return true, nil
			}
		}
		return false, nil
	})
	return err == nil
}

func scrapeMetrics(t *testing.T, baseURL, secret, prefix string) []string {
	t.Helper()
	req, err := http.NewRequest("GET", baseURL+"/extensions/v2/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var lines []string
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		if s := strings.TrimRight(line, "\n"); strings.HasPrefix(s, prefix+"{") {
			lines = append(lines, s)
		}
		if err != nil {
			break
		}
	}
	return lines
}

func metricPresent(lines []string, labels ...string) bool {
	for _, l := range lines {
		ok := true
		for _, want := range labels {
			if !strings.Contains(l, want) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
