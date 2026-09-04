package configuration

import (
	"strings"
	"testing"
)

const imageStreamsBaseYaml = `
version: 0.1
storage:
  inmemory: {}
middleware:
  registry:
    - name: openshift
  repository:
    - name: openshift
  storage:
    - name: openshift
openshift:
  version: 1.0
  server:
    addr: localhost:5000
`

func TestImageStreamsEnabledDefault(t *testing.T) {
	_, cfg, err := Parse(strings.NewReader(imageStreamsBaseYaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageStreams == nil || !cfg.ImageStreams.Enabled {
		t.Fatalf("expected imagestreams enabled by default, got %#v", cfg.ImageStreams)
	}
	// With imagestreams enabled the pullthrough clamp still applies.
	if !cfg.Pullthrough.Enabled {
		t.Fatalf("expected pullthrough enabled by default")
	}
}

func TestImageStreamsDisabledRelaxesPullthrough(t *testing.T) {
	yaml := imageStreamsBaseYaml + "  imagestreams:\n    enabled: false\n  pullthrough:\n    enabled: false\n"

	_, cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageStreams == nil || cfg.ImageStreams.Enabled {
		t.Fatalf("expected imagestreams disabled, got %#v", cfg.ImageStreams)
	}
	// The forced-true clamp must be relaxed when imagestreams are disabled.
	if cfg.Pullthrough.Enabled {
		t.Fatalf("expected pullthrough to stay disabled when imagestreams are disabled")
	}
}
