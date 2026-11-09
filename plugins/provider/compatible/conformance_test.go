package compatible_test

import (
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/plugin/plugintest"
	"github.com/initxy/plugrelay/plugins/provider/bedrock"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/internal/cloudtest"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
	"github.com/initxy/plugrelay/plugins/provider/vertex"
)

// conformanceSkips lists manifests (by id) that cannot run the conformance
// suite, each with the reason. Keep it empty unless a manifest uses an auth
// scheme the suite cannot observe (request signing, token exchange), and
// then set SkipSecretCheck rather than skipping whole scenarios.
var conformanceSkips = map[string]string{}

// TestManifestConformance runs the provider conformance suite over every
// endpoint of every official manifest, so a new file in manifests/ is
// covered without registering it anywhere.
func TestManifestConformance(t *testing.T) {
	files, err := fs.Glob(manifests.FS, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded manifests")
	}
	for _, name := range files {
		if strings.HasSuffix(name, compatible.PricingFileSuffix) {
			continue
		}
		m, err := manifest.Load(manifests.FS, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Run(m.ID, func(t *testing.T) {
			if why, ok := conformanceSkips[m.ID]; ok {
				t.Skip(why)
			}
			for i := range m.Endpoints {
				e := m.Endpoints[i]
				t.Run(e.ID, func(t *testing.T) {
					t.Parallel()
					secret := "sk-conformance"
					suite := plugintest.ProviderSuite{
						Protocol:       e.Protocol,
						FirstByteDelay: 100 * time.Millisecond,
					}
					switch m.EndpointInterpreter(&e) {
					case "bedrock":
						// Access keys sign the request and never travel.
						secret = "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
						fx := cloudtest.BedrockFixtures()
						suite.Fixtures, suite.UseSend, suite.SkipSecretCheck = &fx, true, true
					case "vertex":
						// The key file signs the token request; the upstream
						// sees the access token it was traded for.
						secret = cloudtest.NewTokenServer(t).ServiceAccount
						suite.SkipSecretCheck = true
					}
					suite.New = func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
						return conformanceUpstream(t, name, e.ID, baseURL, secret)
					}
					plugintest.RunProvider(t, suite)
				})
			}
		})
	}
}

// conformanceUpstream builds the provider for endpoint epID of manifest
// file name, with every endpoint re-pointed at the suite's fake upstream.
//
// Variables are filled the way an operator would: a variable that is the
// whole base URL gets baseURL itself, every other required variable a
// placeholder value. The expanded base URL then keeps its path (so
// version prefixes like /openai/v1 still reach the wire) but takes the fake
// upstream's scheme and host.
func conformanceUpstream(t *testing.T, name, epID, baseURL, secret string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
	t.Helper()
	// Load a fresh copy per call: the endpoints are rewritten below.
	m, err := manifest.Load(manifests.FS, name)
	if err != nil {
		t.Fatal(err)
	}
	fake, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	vs := map[string]string{}
	for v, d := range m.Variables {
		val := d.Default
		if val == "" {
			val = "conformance"
		}
		for _, e := range m.Endpoints {
			if strings.TrimSpace(e.BaseURL) == "{"+v+"}" {
				val = baseURL
			}
		}
		cfg[v] = val
		vs[v] = val
	}
	var ep *manifest.Endpoint
	for i := range m.Endpoints {
		e := &m.Endpoints[i]
		u, err := url.Parse(manifest.Expand(e.BaseURL, vs))
		if err != nil {
			t.Fatalf("endpoint %s: base URL: %v", e.ID, err)
		}
		u.Scheme, u.Host = fake.Scheme, fake.Host
		e.BaseURL = u.String()
		if e.ID == epID {
			ep = e
		}
	}
	if ep == nil {
		t.Fatalf("manifest %s has no endpoint %s", m.ID, epID)
	}
	cfg["manifest"] = m
	var p plugin.Provider
	switch in := m.EndpointInterpreter(ep); in {
	case "bedrock":
		p = bedrock.New(compatible.WithClock(time.Now), compatible.WithHTTPClient(http.DefaultClient))
	case "vertex":
		p = vertex.New(compatible.WithClock(time.Now), compatible.WithHTTPClient(http.DefaultClient))
	default:
		p = compatible.New(in, compatible.WithClock(time.Now))
	}
	u := &plugin.Upstream{ID: "up-" + m.ID, Provider: p.Info().ID, Config: cfg}
	// UpstreamID binds outcomes to the manifest, so its error rules apply.
	return p, u, &plugin.Credential{ID: "cred", UpstreamID: u.ID, Secret: secret}
}

// keyOptionalManifests are the official manifests whose server may run
// without a key. Marking any other manifest key-optional would send its
// requests with no auth once its last credential is deleted, so a new
// entry here is a deliberate review decision.
var keyOptionalManifests = map[string]bool{"litellm": true, "bifrost": true, "openai-compatible": true}

func TestKeyOptionalOnlyWhereNoKeyIsNeeded(t *testing.T) {
	files, err := fs.Glob(manifests.FS, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, compatible.PricingFileSuffix) {
			continue
		}
		m, err := manifest.Load(manifests.FS, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		allowed := m.Category == "self_hosted" || keyOptionalManifests[m.ID]
		if m.Auth.KeyOptional && !allowed {
			t.Errorf("%s (category %s) is key-optional; only self-hosted servers and %v may be", m.ID, m.Category, keyOptionalManifests)
		}
		if m.Category == "self_hosted" && !m.Auth.KeyOptional {
			t.Errorf("%s is self-hosted but not key-optional", m.ID)
		}
	}
}
