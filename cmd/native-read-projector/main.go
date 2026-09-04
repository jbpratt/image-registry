// Command native-read-projector is the workstream 4 POC stub: it runs the
// level-triggered ImageStream projection controller against a real cluster. It
// wires the notification intake, the work queue, and a real image clientset. The
// native tag-store adapter is intentionally a TODO (see nativeTagLister) — the
// controller mechanics, not a production tag source, are what this stub exists to
// exercise. See NATIVE-READ-POC.md workstream 4.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	imageclient "github.com/openshift/client-go/image/clientset/versioned"
	"github.com/openshift/image-registry/pkg/projection"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// nativeTagLister is a placeholder. TODO: implement over distribution's storage
// tag store (registry.Repository.Tags(...).All), a ~20-line adapter.
type nativeTagLister struct{}

func (nativeTagLister) Tags(context.Context, string) (map[string]digest.Digest, error) {
	return nil, errors.New("native tag store adapter not implemented (POC stub)")
}

func main() {
	listen := flag.String("listen", ":8080", "address for the notification and metrics HTTP server")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster config")
	registryHost := flag.String("registry-host", "", "host used in TagEvent.DockerImageReference")
	resync := flag.Duration("resync", 30*time.Second, "full resync period")
	workers := flag.Int("workers", 2, "number of reconcile workers")
	flag.Parse()

	cfg, err := restConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("build rest config: %v", err)
	}
	client, err := imageclient.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("build image client: %v", err)
	}

	controller := projection.NewController(nativeTagLister{}, client, projection.Options{
		RegistryHost: *registryHost,
		Workers:      *workers,
		Resync:       *resync,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/", controller.Handler())
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: *listen, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	go controller.Run(ctx)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
