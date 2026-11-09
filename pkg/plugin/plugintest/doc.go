// Package plugintest is the conformance suite for provider plugins.
//
// RunProvider drives any plugin.Provider — built in, manifest driven or an
// external process — against a scripted httptest upstream and asserts the
// verdicts and header handling plugrelay relies on:
//
//	func TestConformance(t *testing.T) {
//		plugintest.RunProvider(t, plugintest.ProviderSuite{
//			Protocol: "openai-chat",
//			New: func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
//				u := &plugin.Upstream{ID: "up", Endpoints: []plugin.Endpoint{{ID: "e", Protocol: "openai-chat", BaseURL: baseURL}}}
//				return myprovider.New(), u, &plugin.Credential{ID: "c", Secret: "sk-test"}
//			},
//		})
//	}
//
// By default the suite plays the core: it builds the request, calls
// Prepare, sends it with its own client, hands failures to Classify with the
// Signal the protocol's codec would produce, and checks the Verdict. With
// UseSend it calls the provider's Send instead and also checks the event
// stream it emits.
//
// Scenarios: normal, stream, header_ownership, cut_stream, rate_limit (429
// with retry-after), unauthorized (401), suspended (403, quarantine without
// retry), content_violation (no retry on any credential), uncertain
// (conservative: quarantine, no retry), timeout_before_send (Sent=false),
// slow_first_byte, oversized_event, error_passthrough and server_error.
// Error bodies are minimal per protocol family; see FixturesFor.
package plugintest
