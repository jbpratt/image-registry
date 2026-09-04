package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/notifications"
	"github.com/opencontainers/go-digest"

	imagefake "github.com/openshift/client-go/image/clientset/versioned/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

type fakeTagLister struct {
	mu   sync.Mutex
	data map[string]map[string]digest.Digest
}

func newFakeTagLister() *fakeTagLister {
	return &fakeTagLister{data: map[string]map[string]digest.Digest{}}
}

func (f *fakeTagLister) set(repo, tag string, d digest.Digest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data[repo] == nil {
		f.data[repo] = map[string]digest.Digest{}
	}
	f.data[repo][tag] = d
}

func (f *fakeTagLister) Tags(_ context.Context, repo string) (map[string]digest.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]digest.Digest, len(f.data[repo]))
	for k, v := range f.data[repo] {
		out[k] = v
	}
	return out, nil
}

// drain synchronously processes the queue to empty, so tests assert against a
// settled state and exact write counts.
func drain(c *Controller) {
	for c.queue.Len() > 0 {
		c.processNext(context.Background())
	}
}

func pushBody(repos ...string) []byte {
	var env envelope
	for _, r := range repos {
		var ev notifications.Event
		ev.Action = notifications.EventActionPush
		ev.Target.Repository = r
		env.Events = append(env.Events, ev)
	}
	b, _ := json.Marshal(env)
	return b
}

func post(t *testing.T, h http.Handler, body []byte) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("handler returned %d", rr.Code)
	}
}

func countStatusWrites(cs *imagefake.Clientset, n *int) {
	cs.PrependReactor("update", "imagestreams", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "status" {
			*n++
		}
		return false, nil, nil
	})
}

func getStream(t *testing.T, cs *imagefake.Clientset, repo string) map[string]string {
	t.Helper()
	ns, name, _ := splitRepo(repo)
	is, err := cs.ImageV1().ImageStreams(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", repo, err)
	}
	heads := map[string]string{}
	for _, ntl := range is.Status.Tags {
		if len(ntl.Items) > 0 {
			heads[ntl.Tag] = ntl.Items[0].Image
		}
	}
	return heads
}

// 1. push event adds the tag; an identical event is a no-op status write.
func TestPushEventAddsTagThenNoOp(t *testing.T) {
	d1 := digest.FromString("img-1")
	tl := newFakeTagLister()
	tl.set("ns/app", "latest", d1)

	cs := imagefake.NewSimpleClientset()
	writes := 0
	countStatusWrites(cs, &writes)

	c := NewController(tl, cs, Options{RegistryHost: "registry:5000"})
	h := c.Handler()

	post(t, h, pushBody("ns/app"))
	drain(c)

	is, err := cs.ImageV1().ImageStreams("ns").Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get imagestream: %v", err)
	}
	if len(is.Status.Tags) != 1 || is.Status.Tags[0].Tag != "latest" {
		t.Fatalf("unexpected status tags: %+v", is.Status.Tags)
	}
	if got := is.Status.Tags[0].Items[0].Image; got != d1.String() {
		t.Fatalf("image = %q, want %q", got, d1.String())
	}
	if got, want := is.Status.Tags[0].Items[0].DockerImageReference, "registry:5000/ns/app@"+d1.String(); got != want {
		t.Fatalf("dockerImageReference = %q, want %q", got, want)
	}
	if writes != 1 {
		t.Fatalf("status writes after first event = %d, want 1", writes)
	}

	post(t, h, pushBody("ns/app"))
	drain(c)
	if writes != 1 {
		t.Fatalf("status writes after identical event = %d, want 1 (no-op expected)", writes)
	}
}

// 2. chaos: 100 pushes across 10 repos with 30% dropped before enqueue; after
// one resync every tag converges.
func TestChaosDropConvergesAfterResync(t *testing.T) {
	tl := newFakeTagLister()
	var repos []string
	for i := 0; i < 10; i++ {
		repo := fmt.Sprintf("ns/app%d", i)
		repos = append(repos, repo)
		for j := 0; j < 10; j++ {
			tl.set(repo, fmt.Sprintf("t%d", j), digest.FromString(fmt.Sprintf("%s-%d", repo, j)))
		}
	}

	cs := imagefake.NewSimpleClientset()
	c := NewController(tl, cs, Options{RegistryHost: "r:5000", ResyncRepos: repos})
	// Deterministic 30% drop.
	var n int
	c.dropFunc = func() bool {
		n++
		return n%10 < 3
	}
	h := c.Handler()

	for i := 0; i < 10; i++ {
		for j := 0; j < 10; j++ {
			post(t, h, pushBody(repos[i]))
		}
	}
	drain(c)

	c.resyncAll()
	drain(c)

	for _, repo := range repos {
		heads := getStream(t, cs, repo)
		if len(heads) != 10 {
			t.Fatalf("%s converged to %d tags, want 10", repo, len(heads))
		}
		for j := 0; j < 10; j++ {
			tag := fmt.Sprintf("t%d", j)
			want := digest.FromString(fmt.Sprintf("%s-%d", repo, j)).String()
			if heads[tag] != want {
				t.Fatalf("%s %s head = %q, want %q", repo, tag, heads[tag], want)
			}
		}
	}
}

// 3. reorder: reconcile ignores event content, so the final state matches the
// TagLister truth regardless of how many events arrive or in what order.
func TestReorderConvergesToTruth(t *testing.T) {
	repo := "ns/app"
	d1 := digest.FromString("v1")
	d2 := digest.FromString("v2")

	tl := newFakeTagLister()
	tl.set(repo, "latest", d1)

	cs := imagefake.NewSimpleClientset()
	c := NewController(tl, cs, Options{RegistryHost: "r:5000"})
	h := c.Handler()

	post(t, h, pushBody(repo))
	drain(c)
	if got := getStream(t, cs, repo)["latest"]; got != d1.String() {
		t.Fatalf("head = %q, want %q", got, d1.String())
	}

	// Truth advances; deliver several events, order/content irrelevant.
	tl.set(repo, "latest", d2)
	post(t, h, pushBody(repo))
	post(t, h, pushBody(repo))
	drain(c)

	ns, name, _ := splitRepo(repo)
	is, err := cs.ImageV1().ImageStreams(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	items := is.Status.Tags[0].Items
	if items[0].Image != d2.String() {
		t.Fatalf("head = %q, want %q", items[0].Image, d2.String())
	}
	if len(items) != 2 || items[1].Image != d1.String() {
		t.Fatalf("history = %+v, want [d2 d1]", items)
	}
}

// 4. restart: a fresh controller with an empty queue converges from the tag
// store alone via resync (seen is empty on restart, hence the injectable list).
func TestRestartConvergesFromTagLister(t *testing.T) {
	repo := "ns/app"
	d := digest.FromString("x")
	tl := newFakeTagLister()
	tl.set(repo, "latest", d)

	cs := imagefake.NewSimpleClientset()
	c := NewController(tl, cs, Options{RegistryHost: "r:5000", ResyncRepos: []string{repo}})

	c.resyncAll()
	drain(c)

	if got := getStream(t, cs, repo)["latest"]; got != d.String() {
		t.Fatalf("head = %q, want %q", got, d.String())
	}
}

// 5. lag: record p99 of event-to-write latency over the load from test 2's shape
// and assert it stays under the fakes bound.
func TestReconcileLagP99(t *testing.T) {
	tl := newFakeTagLister()
	var repos []string
	for i := 0; i < 10; i++ {
		repo := fmt.Sprintf("ns/app%d", i)
		repos = append(repos, repo)
		for j := 0; j < 10; j++ {
			tl.set(repo, fmt.Sprintf("t%d", j), digest.FromString(fmt.Sprintf("%s-%d", repo, j)))
		}
	}

	cs := imagefake.NewSimpleClientset()
	c := NewController(tl, cs, Options{RegistryHost: "r:5000"})
	var lags []time.Duration
	c.onLag = func(d time.Duration) { lags = append(lags, d) }
	h := c.Handler()

	for i := 0; i < 10; i++ {
		for j := 0; j < 10; j++ {
			post(t, h, pushBody(repos[i]))
		}
	}
	drain(c)

	if len(lags) == 0 {
		t.Fatal("no lag samples recorded")
	}
	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	p99 := lags[(len(lags)*99+99)/100-1]
	t.Logf("projection reconcile lag p99 (fakes) = %v over %d samples", p99, len(lags))
	if p99 >= 2*time.Second {
		t.Fatalf("lag p99 = %v, want < 2s", p99)
	}
}
