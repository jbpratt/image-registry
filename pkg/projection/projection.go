// Package projection is the workstream 4 POC stub from NATIVE-READ-POC.md: a
// level-triggered controller that projects the native tag store into ImageStream
// status. It is an experiment, not an operator. Events are hints only (fact 5);
// reconcile re-lists the tag store as truth. It never removes tags an event
// cannot vouch for: a native listing is incomplete by construction (fact 3/6),
// so the stub only adds or updates status tags and leaves etcd-owned tags alone.
package projection

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"

	imagev1 "github.com/openshift/api/image/v1"
	imageclient "github.com/openshift/client-go/image/clientset/versioned"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// TagLister returns the tags a repository resolves to in the native storage
// layout. It is the source of truth for reconcile. Production would implement it
// as a thin adapter over distribution's storage tag store; tests use a fake.
type TagLister interface {
	Tags(ctx context.Context, repo string) (map[string]digest.Digest, error)
}

// Options configures a Controller.
type Options struct {
	// RegistryHost is the host used to build TagEvent.DockerImageReference.
	RegistryHost string
	// Workers is the number of concurrent reconcile workers.
	Workers int
	// Resync is the full-resync period; <= 0 disables periodic resync.
	Resync time.Duration
	// ResyncRepos are repos always included in a resync, on top of every repo
	// seen so far. A restarted controller has an empty seen set, so this list
	// is how it converges from the tag store alone (fact 5).
	ResyncRepos []string
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// Controller consumes push/delete notification events and reconciles ImageStream
// status from the native tag store.
type Controller struct {
	tags    TagLister
	client  imageclient.Interface
	host    string
	workers int
	resync  time.Duration
	now     func() time.Time
	queue   workqueue.TypedRateLimitingInterface[string]

	// dropFunc, when set, drops an event before enqueue and counts it. It is
	// the chaos hook exercised by the drop test.
	dropFunc func() bool
	// onLag, when set, receives every observed event-to-write latency. Test seam.
	onLag func(time.Duration)

	mu      sync.Mutex
	seen    map[string]struct{}
	pending map[string]time.Time
}

// NewController wires a Controller. In the binary client is a real clientset; in
// tests it is the versioned fake.
func NewController(tags TagLister, client imageclient.Interface, opts Options) *Controller {
	now := opts.now
	if now == nil {
		now = time.Now
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 1
	}
	c := &Controller{
		tags:    tags,
		client:  client,
		host:    opts.RegistryHost,
		workers: workers,
		resync:  opts.Resync,
		now:     now,
		queue: workqueue.NewTypedRateLimitingQueue[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
		),
		seen:    map[string]struct{}{},
		pending: map[string]time.Time{},
	}
	for _, r := range opts.ResyncRepos {
		c.seen[r] = struct{}{}
	}
	return c
}

// enqueue records the repo as seen, stamps its first pending event time (for the
// lag metric), and queues it. Events carry no data into reconcile.
func (c *Controller) enqueue(repo string, receivedAt time.Time) {
	c.mu.Lock()
	c.seen[repo] = struct{}{}
	if _, ok := c.pending[repo]; !ok {
		c.pending[repo] = receivedAt
	}
	c.mu.Unlock()
	c.queue.Add(repo)
}

// resyncAll enqueues every repo seen so far. Resync carries no event time, so it
// does not feed the lag metric.
func (c *Controller) resyncAll() {
	c.mu.Lock()
	repos := make([]string, 0, len(c.seen))
	for r := range c.seen {
		repos = append(repos, r)
	}
	c.mu.Unlock()
	for _, r := range repos {
		c.queue.Add(r)
	}
}

// Run starts the workers and the resync loop and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) {
	if c.resync > 0 {
		go c.resyncLoop(ctx)
	}
	var wg sync.WaitGroup
	for i := 0; i < c.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	c.queue.ShutDown()
	wg.Wait()
}

func (c *Controller) resyncLoop(ctx context.Context) {
	t := time.NewTicker(c.resync)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.resyncAll()
		}
	}
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	result, err := c.reconcile(ctx, key)
	reconcileTotal.WithLabelValues(result).Inc()
	c.recordLag(key, err)
	if err != nil {
		klog.Warningf("projection: reconcile %q: %v", key, err)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// recordLag observes event-to-write latency for an event-triggered reconcile and
// clears the pending stamp so resync-only reconciles do not skew the metric.
func (c *Controller) recordLag(key string, reconcileErr error) {
	if reconcileErr != nil {
		return
	}
	c.mu.Lock()
	t, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if ok {
		lag := c.now().Sub(t)
		reconcileLagSeconds.Observe(lag.Seconds())
		if c.onLag != nil {
			c.onLag(lag)
		}
	}
}

// reconcile lists the native tags for repo (truth) and patches the ImageStream
// status to include them, creating the stream if absent. It never removes tags:
// the native listing is incomplete, so a tag missing from it may still be
// etcd-owned. Spec is never touched.
func (c *Controller) reconcile(ctx context.Context, repo string) (string, error) {
	ns, name, ok := splitRepo(repo)
	if !ok {
		return "noop", nil
	}
	tags, err := c.tags.Tags(ctx, repo)
	if err != nil {
		return "error", err
	}

	streams := c.client.ImageV1().ImageStreams(ns)
	created := false
	is, err := streams.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// The namespace must already exist; the stub does not create it.
		is, err = streams.Create(ctx, &imagev1.ImageStream{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		}, metav1.CreateOptions{})
		if err != nil {
			return "error", err
		}
		created = true
	} else if err != nil {
		return "error", err
	}

	desired := c.desiredStatusTags(is.Status.Tags, tags, repo)
	if reflect.DeepEqual(is.Status.Tags, desired) {
		if created {
			return "created", nil
		}
		return "noop", nil
	}

	upd := is.DeepCopy()
	upd.Status.Tags = desired
	if _, err := streams.UpdateStatus(ctx, upd, metav1.UpdateOptions{}); err != nil {
		return "error", err
	}
	if created {
		return "created", nil
	}
	return "updated", nil
}

// desiredStatusTags merges the native tags into the existing status. A native tag
// whose current head already points at the same digest is left byte-for-byte
// unchanged so an identical event is a no-op; a changed or new tag gets a fresh
// TagEvent prepended. Existing tags absent from the native store are preserved.
func (c *Controller) desiredStatusTags(existing []imagev1.NamedTagEventList, native map[string]digest.Digest, repo string) []imagev1.NamedTagEventList {
	out := make([]imagev1.NamedTagEventList, len(existing))
	copy(out, existing)

	idx := make(map[string]int, len(existing))
	for i, ntl := range existing {
		idx[ntl.Tag] = i
	}

	now := metav1.NewTime(c.now())
	event := func(d digest.Digest) imagev1.TagEvent {
		return imagev1.TagEvent{
			Created:              now,
			DockerImageReference: c.host + "/" + repo + "@" + d.String(),
			Image:                d.String(),
		}
	}

	var newTags []string
	for tag, d := range native {
		i, ok := idx[tag]
		if !ok {
			newTags = append(newTags, tag)
			continue
		}
		cur := existing[i]
		if len(cur.Items) > 0 && cur.Items[0].Image == d.String() {
			continue
		}
		updated := cur
		updated.Items = append([]imagev1.TagEvent{event(d)}, cur.Items...)
		out[i] = updated
	}

	sort.Strings(newTags)
	for _, tag := range newTags {
		out = append(out, imagev1.NamedTagEventList{
			Tag:   tag,
			Items: []imagev1.TagEvent{event(native[tag])},
		})
	}
	return out
}

func splitRepo(repo string) (ns, name string, ok bool) {
	i := strings.IndexByte(repo, '/')
	if i <= 0 || i == len(repo)-1 {
		return "", "", false
	}
	return repo[:i], repo[i+1:], true
}
