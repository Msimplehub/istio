// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xds

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	anypb "google.golang.org/protobuf/types/known/anypb"

	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	security "istio.io/api/security/v1beta1"
	"istio.io/api/type/v1beta1"
	"istio.io/istio/pilot/pkg/config/kube/crd"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/route"
	"istio.io/istio/pilot/pkg/util/protoconv"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/env"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/spiffe"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/yml"
	"istio.io/istio/pkg/util/protomarshal"
	"istio.io/istio/pkg/util/sets"
)

// ConfigInput defines inputs passed to the test config templates
// This allows tests to do things like create a virtual service for each service, for example
type ConfigInput struct {
	// Name of the test
	Name string
	// Name of the test config file to use. If not set, <Name> is used
	ConfigName string
	// Number of services to make
	Services int
	// Number of instances to make
	Instances int
	// ResourceType of proxy to generate configs for
	ProxyType model.NodeType
}

var testCases = []ConfigInput{
	{
		// Gateways provides an example config for a large Ingress deployment. This will create N
		// virtual services and gateways, where routing is determined by hostname, meaning we generate N routes for HTTPS.
		Name:      "gateways",
		Services:  1000,
		ProxyType: model.Router,
	},
	{
		// Gateways-shared provides an example config for a large Ingress deployment. This will create N
		// virtual services and gateways, where routing is determined by path. This means there will be a single large route.
		Name:      "gateways-shared",
		Services:  1000,
		ProxyType: model.Router,
	},
	{
		Name:      "empty",
		Services:  100,
		ProxyType: model.SidecarProxy,
	},
	{
		Name:     "tls",
		Services: 100,
	},
	{
		Name:     "telemetry",
		Services: 100,
	},
	{
		Name:     "telemetry-api",
		Services: 100,
	},
	{
		Name:     "virtualservice",
		Services: 100,
	},
	{
		Name:     "authorizationpolicy",
		Services: 100,
	},
	{
		Name:     "peerauthentication",
		Services: 100,
	},
	{
		Name:      "knative-gateway",
		Services:  100,
		ProxyType: model.Router,
	},
	{
		Name:      "serviceentry-workloadentry",
		Services:  100,
		Instances: 1000,
		ProxyType: model.SidecarProxy,
	},
}

var sidecarTestCases = func() (res []ConfigInput) {
	for _, c := range testCases {
		if c.ProxyType == model.Router {
			continue
		}
		res = append(res, c)
	}
	return res
}()

func configureBenchmark(t test.Failer) {
	for _, s := range istiolog.Scopes() {
		if s.Name() == benchmarkScope.Name() {
			continue
		}
		s.SetOutputLevel(istiolog.NoneLevel)
	}
	test.SetForTest(t, &features.EnableXDSCaching, false)
}

func BenchmarkInitPushContext(b *testing.B) {
	configureBenchmark(b)
	for _, tt := range testCases {
		b.Run(tt.Name, func(b *testing.B) {
			s, proxy := setupTest(b, tt)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				s.Env().PushContext().InitDone.Store(false)
				initPushContext(s.Env(), proxy)
			}
		})
	}
}

// Do a quick sanity tests to make sure telemetry v2 filters are applying. This ensures as they
// update our benchmark doesn't become useless.
func TestValidateTelemetry(t *testing.T) {
	s, proxy := setupAndInitializeTest(t, ConfigInput{Name: "telemetry", Services: 1})
	c, _, _ := s.Discovery.Generators[v3.ClusterType].Generate(proxy, nil, &model.PushRequest{Full: true, Push: s.PushContext()})
	if len(c) == 0 {
		t.Fatal("Got no clusters!")
	}
	for _, r := range c {
		cls := &cluster.Cluster{}
		if err := r.GetResource().UnmarshalTo(cls); err != nil {
			t.Fatal(err)
		}
		for _, ff := range cls.Filters {
			if ff.Name == "istio.metadata_exchange" {
				return
			}
		}
	}
	t.Fatalf("telemetry v2 filters not found")
}

func BenchmarkRouteGeneration(b *testing.B) {
	runBenchmark(b, v3.RouteType, testCases)
}

func TestRouteGeneration(t *testing.T) {
	testBenchmark(t, v3.RouteType, testCases)
}

func BenchmarkClusterGeneration(b *testing.B) {
	runBenchmark(b, v3.ClusterType, testCases)
}

func TestClusterGeneration(t *testing.T) {
	testBenchmark(t, v3.ClusterType, testCases)
}

func BenchmarkListenerGeneration(b *testing.B) {
	runBenchmark(b, v3.ListenerType, testCases)
}

func TestListenerGeneration(t *testing.T) {
	testBenchmark(t, v3.ListenerType, testCases)
}

func BenchmarkNameTableGeneration(b *testing.B) {
	runBenchmark(b, v3.NameTableType, sidecarTestCases)
}

func TestNameTableGeneration(t *testing.T) {
	testBenchmark(t, v3.NameTableType, sidecarTestCases)
}

var secretCases = []ConfigInput{
	{
		Name:      "secrets",
		Services:  10,
		ProxyType: model.Router,
	},
	{
		Name:      "secrets",
		Services:  1000,
		ProxyType: model.Router,
	},
}

func TestSecretGeneration(t *testing.T) {
	testBenchmark(t, v3.SecretType, secretCases)
}

func BenchmarkSecretGeneration(b *testing.B) {
	runBenchmark(b, v3.SecretType, secretCases)
}

func createGateways(n int) map[string]*meshconfig.Network {
	out := make(map[string]*meshconfig.Network, n)
	for i := 0; i < n; i++ {
		out[fmt.Sprintf("network-%d", i)] = &meshconfig.Network{
			Gateways: []*meshconfig.Network_IstioNetworkGateway{{
				Gw:   &meshconfig.Network_IstioNetworkGateway_Address{Address: fmt.Sprintf("35.0.0.%d", i)},
				Port: 15443,
			}},
		}
	}
	return out
}

// BenchmarkEDS measures performance of EDS config generation
// TODO Add more variables, such as different services
func BenchmarkEndpointGeneration(b *testing.B) {
	configureBenchmark(b)

	const numNetworks = 4
	tests := []struct {
		endpoints int
		services  int
	}{
		{1, 100},
		{10, 10},
		{100, 10},
		{1000, 1},
	}

	var response *discovery.DiscoveryResponse
	for _, tt := range tests {
		b.Run(fmt.Sprintf("%d/%d", tt.endpoints, tt.services), func(b *testing.B) {
			s := NewFakeDiscoveryServer(b, FakeOptions{
				Configs: createEndpointsConfig(tt.endpoints, tt.services, numNetworks),
				NetworksWatcher: mesh.NewFixedNetworksWatcher(&meshconfig.MeshNetworks{
					Networks: createGateways(numNetworks),
				}),
			})
			proxy := &model.Proxy{
				Type:            model.SidecarProxy,
				IPAddresses:     []string{"10.3.3.3"},
				ID:              "random",
				ConfigNamespace: "default",
				Metadata:        &model.NodeMetadata{},
			}
			push := s.PushContext()
			proxy.SetSidecarScope(push)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				loadAssignments := make([]*anypb.Any, 0)
				for svc := 0; svc < tt.services; svc++ {
					l := s.Discovery.generateEndpoints(NewEndpointBuilder(fmt.Sprintf("outbound|80||foo-%d.com", svc), proxy, push))
					loadAssignments = append(loadAssignments, protoconv.MessageToAny(l))
				}
				response = endpointDiscoveryResponse(loadAssignments, push.PushVersion, push.LedgerVersion)
			}
			logDebug(b, model.AnyToUnnamedResources(response.GetResources()))
		})
	}
}

func runBenchmark(b *testing.B, tpe string, testCases []ConfigInput) {
	configureBenchmark(b)
	for _, tt := range testCases {
		b.Run(tt.Name, func(b *testing.B) {
			s, proxy := setupAndInitializeTest(b, tt)
			wr := getWatchedResources(tpe, tt, s, proxy)
			b.ResetTimer()
			var c model.Resources
			for n := 0; n < b.N; n++ {
				c, _, _ = s.Discovery.Generators[tpe].Generate(proxy, wr, &model.PushRequest{Full: true, Push: s.PushContext()})
				if len(c) == 0 {
					b.Fatalf("Got no %v's!", tpe)
				}
			}
			logDebug(b, c)
		})
	}
}

func testBenchmark(t *testing.T, tpe string, testCases []ConfigInput) {
	for _, tt := range testCases {
		t.Run(tt.Name, func(t *testing.T) {
			// No need for large test here
			tt.Services = 1
			tt.Instances = 1
			s, proxy := setupAndInitializeTest(t, tt)
			wr := getWatchedResources(tpe, tt, s, proxy)
			c, _, _ := s.Discovery.Generators[tpe].Generate(proxy, wr, &model.PushRequest{Full: true, Push: s.PushContext()})
			if len(c) == 0 {
				t.Fatalf("Got no %v's!", tpe)
			}
		})
	}
}

func getWatchedResources(tpe string, tt ConfigInput, s *FakeDiscoveryServer, proxy *model.Proxy) *model.WatchedResource {
	switch tpe {
	case v3.SecretType:
		watchedResources := []string{}
		for i := 0; i < tt.Services; i++ {
			watchedResources = append(watchedResources, fmt.Sprintf("kubernetes://default/sds-credential-%d", i))
		}
		return &model.WatchedResource{ResourceNames: watchedResources}
	case v3.RouteType:
		l := s.Discovery.ConfigGenerator.BuildListeners(proxy, s.PushContext())
		routeNames := xdstest.ExtractRoutesFromListeners(l)
		return &model.WatchedResource{ResourceNames: routeNames}
	}
	return nil
}

// Setup test builds a mock test environment. Note: push context is not initialized, to be able to benchmark separately
// most should just call setupAndInitializeTest
func setupTest(t testing.TB, config ConfigInput) (*FakeDiscoveryServer, *model.Proxy) {
	proxyType := config.ProxyType
	if proxyType == "" {
		proxyType = model.SidecarProxy
	}
	proxy := &model.Proxy{
		Type:        proxyType,
		IPAddresses: []string{"1.1.1.1"},
		ID:          "v0.default",
		DNSDomain:   "default.example.org",
		Labels: map[string]string{
			"istio.io/benchmark": "true",
		},
		Metadata: &model.NodeMetadata{
			Namespace: "default",
			Labels: map[string]string{
				"istio.io/benchmark": "true",
			},
			ClusterID:    "Kubernetes",
			IstioVersion: "1.19.0",
		},
		ConfigNamespace:  "default",
		VerifiedIdentity: &spiffe.Identity{Namespace: "default"},
	}
	proxy.IstioVersion = model.ParseIstioVersion(proxy.Metadata.IstioVersion)
	// need to call DiscoverIPMode to check the ipMode of the proxy
	proxy.DiscoverIPMode()

	configs, k8sConfig := getConfigsWithCache(t, config)
	m := mesh.DefaultMeshConfig()
	m.ExtensionProviders = append(m.ExtensionProviders, &meshconfig.MeshConfig_ExtensionProvider{
		Name: "envoy-json",
		Provider: &meshconfig.MeshConfig_ExtensionProvider_EnvoyFileAccessLog{
			EnvoyFileAccessLog: &meshconfig.MeshConfig_ExtensionProvider_EnvoyFileAccessLogProvider{
				Path: "/dev/stdout",
				LogFormat: &meshconfig.MeshConfig_ExtensionProvider_EnvoyFileAccessLogProvider_LogFormat{
					LogFormat: &meshconfig.MeshConfig_ExtensionProvider_EnvoyFileAccessLogProvider_LogFormat_Labels{},
				},
			},
		},
	})
	s := NewFakeDiscoveryServer(t, FakeOptions{
		Configs:                configs,
		KubernetesObjectString: k8sConfig,
		// Allow debounce to avoid overwhelming with writes
		DebounceTime:               time.Millisecond * 10,
		DisableSecretAuthorization: true,
		MeshConfig:                 m,
	})

	return s, proxy
}

var (
	configCache    = map[ConfigInput][]config.Config{}
	k8sConfigCache = map[ConfigInput]string{}
)

func getConfigsWithCache(t testing.TB, input ConfigInput) ([]config.Config, string) {
	// Config setup is slow for large tests. Cache this and return from Cache.
	// This improves even running a single test, as go will run the full test (including setup) at least twice.
	if cached, f := configCache[input]; f {
		return cached, k8sConfigCache[input]
	}
	configName := input.ConfigName
	if configName == "" {
		configName = input.Name
	}
	tmpl := template.Must(template.New("").Funcs(sprig.TxtFuncMap()).ParseFiles(path.Join("testdata", "benchmarks", configName+".yaml")))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, configName+".yaml", input); err != nil {
		t.Fatalf("failed to execute template: %v", err)
	}
	extra := path.Join("testdata", "benchmarks", configName+".extra.yaml")
	inputYAML := buf.String()
	if _, err := os.Stat(extra); err == nil {
		bdata, err := os.ReadFile(extra)
		if err != nil {
			t.Fatal(err)
		}

		inputYAML += "\n---\n" + yml.SplitYamlByKind(string(bdata))[gvk.EnvoyFilter.Kind]
	}

	configs, badKinds, err := crd.ParseInputs(inputYAML)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	scrt, count := parseSecrets(inputYAML)
	if len(badKinds) != count {
		t.Fatalf("Got unknown resources: %v", badKinds)
	}
	// setup default namespace if not defined
	for i, c := range configs {
		if c.Namespace == "" {
			c.Namespace = "default"
		}
		configs[i] = c
	}
	configCache[input] = configs
	k8sConfigCache[input] = scrt
	return configs, scrt
}

func parseSecrets(inputs string) (string, int) {
	matches := 0
	sb := strings.Builder{}
	for _, text := range strings.Split(inputs, "\n---") {
		if strings.Contains(text, "kind: Secret") {
			sb.WriteString(text + "\n---\n")
			matches++
		}
	}
	return sb.String(), matches
}

func setupAndInitializeTest(t testing.TB, config ConfigInput) (*FakeDiscoveryServer, *model.Proxy) {
	s, proxy := setupTest(t, config)
	initPushContext(s.Env(), proxy)
	return s, proxy
}

func initPushContext(env *model.Environment, proxy *model.Proxy) {
	pushContext := env.PushContext()
	pushContext.InitContext(env, nil, nil)
	proxy.SetSidecarScope(pushContext)
	proxy.SetGatewaysForProxy(pushContext)
	proxy.SetServiceInstances(env.ServiceDiscovery)
}

var debugGeneration = env.Register("DEBUG_CONFIG_DUMP", false, "if enabled, print a full config dump of the generated config")

var benchmarkScope = istiolog.RegisterScope("benchmark", "")

// Add additional debug info for a test
func logDebug(b *testing.B, m model.Resources) {
	b.Helper()
	b.StopTimer()

	if debugGeneration.Get() {
		for i, r := range m {
			s, err := protomarshal.MarshalIndent(r, "  ")
			if err != nil {
				b.Fatal(err)
			}
			// Cannot use b.Logf, it truncates
			benchmarkScope.Infof("Generated: %d %s", i, s)
		}
	}
	bytes := 0
	for _, r := range m {
		bytes += len(r.GetResource().Value)
	}
	b.ReportMetric(float64(bytes)/1000, "kb/msg")
	b.ReportMetric(float64(len(m)), "resources/msg")
	b.StartTimer()
}

func createEndpointsConfig(numEndpoints, numServices, numNetworks int) []config.Config {
	result := make([]config.Config, 0, numServices)
	for s := 0; s < numServices; s++ {
		endpoints := make([]*networking.WorkloadEntry, 0, numEndpoints)
		for e := 0; e < numEndpoints; e++ {
			endpoints = append(endpoints, &networking.WorkloadEntry{
				Labels: map[string]string{
					"type": "eds-benchmark",
					"app":  "foo-" + strconv.Itoa(s),
				},
				Address:        fmt.Sprintf("111.%d.%d.%d", e/(256*256), (e/256)%256, e%256),
				Network:        fmt.Sprintf("network-%d", e%numNetworks),
				ServiceAccount: "something",
			})
		}
		result = append(result, config.Config{
			Meta: config.Meta{
				GroupVersionKind:  gvk.ServiceEntry,
				Name:              "foo-" + strconv.Itoa(s),
				Namespace:         "default",
				CreationTimestamp: time.Now(),
			},
			Spec: &networking.ServiceEntry{
				Hosts: []string{fmt.Sprintf("foo-%d.com", s)},
				Ports: []*networking.ServicePort{
					{Number: 80, Name: "http-port", Protocol: "http"},
				},
				Endpoints:  endpoints,
				Resolution: networking.ServiceEntry_STATIC,
			},
		})
	}
	// EDS looks up PA, so add a few...
	result = append(result, config.Config{
		Meta: config.Meta{
			GroupVersionKind:  gvk.PeerAuthentication,
			Name:              "global",
			Namespace:         "istio-system",
			CreationTimestamp: time.Now(),
		},
		Spec: &security.PeerAuthentication{
			Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_PERMISSIVE},
		},
	},
		config.Config{
			Meta: config.Meta{
				GroupVersionKind:  gvk.PeerAuthentication,
				Name:              "namespace",
				Namespace:         "default",
				CreationTimestamp: time.Now(),
			},
			Spec: &security.PeerAuthentication{
				Mtls: &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
			},
		},
		config.Config{
			Meta: config.Meta{
				GroupVersionKind:  gvk.PeerAuthentication,
				Name:              "selector",
				Namespace:         "default",
				CreationTimestamp: time.Now(),
			},
			Spec: &security.PeerAuthentication{
				Selector: &v1beta1.WorkloadSelector{MatchLabels: map[string]string{"type": "eds-benchmark"}},
				Mtls:     &security.PeerAuthentication_MutualTLS{Mode: security.PeerAuthentication_MutualTLS_DISABLE},
			},
		})
	return result
}

func BenchmarkPushRequest(b *testing.B) {
	// allTriggers contains all triggers, so we can pick one at random.
	// It is not a big issue if it falls out of sync, as we are just trying to generate test data
	allTriggers := []model.TriggerReason{
		model.EndpointUpdate,
		model.ConfigUpdate,
		model.ServiceUpdate,
		model.ProxyUpdate,
		model.GlobalUpdate,
		model.UnknownTrigger,
		model.DebugTrigger,
		model.SecretTrigger,
		model.NetworksTrigger,
		model.ProxyRequest,
		model.NamespaceUpdate,
	}
	// Number of (simulated) proxies
	proxies := 500
	// Number of (simulated) pushes merged
	pushesMerged := 10
	// Number of configs per push
	configs := 1

	for n := 0; n < b.N; n++ {
		var req *model.PushRequest
		for i := 0; i < pushesMerged; i++ {
			trigger := allTriggers[i%len(allTriggers)]
			nreq := &model.PushRequest{
				ConfigsUpdated: sets.New[model.ConfigKey](),
				Reason:         []model.TriggerReason{trigger},
			}
			for c := 0; c < configs; c++ {
				nreq.ConfigsUpdated[model.ConfigKey{Kind: kind.ServiceEntry, Name: fmt.Sprintf("%d", c), Namespace: "default"}] = struct{}{}
			}
			req = req.Merge(nreq)
		}
		for p := 0; p < proxies; p++ {
			recordPushTriggers(req.Reason...)
		}
	}
}

func makeCacheKey(n int) model.XdsCacheEntry {
	ns := strconv.Itoa(n)

	// 100 services
	services := make([]*model.Service, 0, 100)
	// 100 destinationrules
	drs := make([]*model.ConsolidatedDestRule, 0, 100)
	for i := 0; i < 100; i++ {
		index := strconv.Itoa(i)
		services = append(services, &model.Service{
			Hostname:   host.Name(ns + "some" + index + ".example.com"),
			Attributes: model.ServiceAttributes{Namespace: "test" + index},
		})
		drs = append(drs, model.ConvertConsolidatedDestRule(&config.Config{Meta: config.Meta{Name: index, Namespace: index}}))
	}

	key := &route.Cache{
		RouteName:        "something",
		ClusterID:        "my-cluster",
		DNSDomain:        "some.domain.example.com",
		DNSCapture:       true,
		DNSAutoAllocate:  false,
		ListenerPort:     1234,
		Services:         services,
		DestinationRules: drs,
		EnvoyFilterKeys:  []string{ns + "1/a", ns + "2/b", ns + "3/c"},
	}
	return key
}

func BenchmarkCache(b *testing.B) {
	// Ensure cache doesn't grow too large
	test.SetForTest(b, &features.XDSCacheMaxSize, 1_000)
	res := &discovery.Resource{Name: "test"}
	zeroTime := time.Time{}
	b.Run("key", func(b *testing.B) {
		key := makeCacheKey(1)
		for n := 0; n < b.N; n++ {
			_ = key.Key()
		}
	})
	b.Run("insert", func(b *testing.B) {
		c := model.NewXdsCache()
		stop := make(chan struct{})
		defer close(stop)
		c.Run(stop)
		for n := 0; n < b.N; n++ {
			key := makeCacheKey(n)
			req := &model.PushRequest{Start: zeroTime.Add(time.Duration(n))}
			c.Add(key, req, res)
		}
	})
	// to trigger clear index on old dependents
	b.Run("insert same key", func(b *testing.B) {
		c := model.NewXdsCache()
		stop := make(chan struct{})
		defer close(stop)
		c.Run(stop)
		// First occupy cache capacity
		for i := 0; i < features.XDSCacheMaxSize; i++ {
			key := makeCacheKey(i)
			req := &model.PushRequest{Start: zeroTime.Add(time.Duration(i))}
			c.Add(key, req, res)
		}
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			key := makeCacheKey(1)
			req := &model.PushRequest{Start: zeroTime.Add(time.Duration(features.XDSCacheMaxSize + n))}
			c.Add(key, req, res)
		}
	})
	b.Run("get", func(b *testing.B) {
		c := model.NewXdsCache()
		key := makeCacheKey(1)
		req := &model.PushRequest{Start: zeroTime.Add(time.Duration(1))}
		c.Add(key, req, res)
		for n := 0; n < b.N; n++ {
			c.Get(key)
		}
	})

	b.Run("insert and get", func(b *testing.B) {
		c := model.NewXdsCache()
		stop := make(chan struct{})
		defer close(stop)
		c.Run(stop)
		// First occupy cache capacity
		for i := 0; i < features.XDSCacheMaxSize; i++ {
			key := makeCacheKey(i)
			req := &model.PushRequest{Start: zeroTime.Add(time.Duration(i))}
			c.Add(key, req, res)
		}
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			key := makeCacheKey(n)
			req := &model.PushRequest{Start: zeroTime.Add(time.Duration(features.XDSCacheMaxSize + n))}
			c.Add(key, req, res)
			c.Get(key)
		}
	})
}
