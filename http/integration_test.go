package http_integration

import (
	"net"

	"crypto/tls"
	"crypto/x509"
	"path"
	"runtime"
	"sync"
	"testing"
	"time"

	"io/ioutil"

	"github.com/mwitkow/go-conntrack/connhelpers"
	"github.com/mwitkow/go-srvlb/srv"
	pb_res "github.com/mwitkow/kedge/_protogen/kedge/config/common/resolvers"
	pb_be "github.com/mwitkow/kedge/_protogen/kedge/config/http/backends"
	pb_route "github.com/mwitkow/kedge/_protogen/kedge/config/http/routes"

	"fmt"

	"context"
	"net/http"
	"net/url"

	"errors"

	"strings"

	"github.com/mwitkow/kedge/http/backendpool"
	"github.com/mwitkow/kedge/http/director"
	"github.com/mwitkow/kedge/http/director/router"
	"github.com/mwitkow/kedge/lib/resolvers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

var backendResolutionDuration = 10 * time.Millisecond

var backendConfigs = []*pb_be.Backend{
	&pb_be.Backend{
		Name: "non_secure",
		Resolver: &pb_be.Backend_Srv{
			Srv: &pb_res.SrvResolver{
				DnsName: "_http._tcp.nonsecure.backends.test.local",
			},
		},
		Balancer: pb_be.Balancer_ROUND_ROBIN,
	},
	&pb_be.Backend{
		Name: "secure",
		Resolver: &pb_be.Backend_Srv{
			Srv: &pb_res.SrvResolver{
				DnsName: "_https._tcp.secure.backends.test.local",
			},
		},
		Security: &pb_be.Security{
			InsecureSkipVerify: true, // TODO(mwitkow): Add config TLS once we do parsing of TLS configs.
		},
		Balancer: pb_be.Balancer_ROUND_ROBIN,
	},
}

var nonSecureBackendCount = 5
var secureBackendCount = 10

var routeConfigs = []*pb_route.Route{
	&pb_route.Route{
		BackendName: "non_secure",
		PathRules:   []string{"/some/strict/path"},
		HostMatcher: "nonsecure.ext.example.com",
		ProxyMode:   pb_route.ProxyMode_REVERSE_PROXY,
	},
	&pb_route.Route{
		BackendName: "non_secure",
		HostMatcher: "nonsecure.backends.test.local",
		ProxyMode:   pb_route.ProxyMode_FORWARD_PROXY,
	},
	&pb_route.Route{
		BackendName: "secure",
		PathRules:   []string{"/some/strict/path"},
		HostMatcher: "secure.ext.example.com",
		ProxyMode:   pb_route.ProxyMode_REVERSE_PROXY,
	},
	&pb_route.Route{
		BackendName: "secure",
		HostMatcher: "secure.backends.test.local",
		ProxyMode:   pb_route.ProxyMode_FORWARD_PROXY,
	},
}

var adhocConfig = []*pb_route.Adhoc{
	{
		DnsNameMatcher: "*.pods.test.local",
		Port: &pb_route.Adhoc_Port{
			AllowedRanges: []*pb_route.Adhoc_Port_Range{
				{
					// This will be started on local host. God knows what port it will be.
					From: 1024,
					To:   65535,
				},
			},
		},
	},
}

func unknownPingbackHandler(serverAddr string) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Set("x-test-req-proto", fmt.Sprintf("%d.%d", req.ProtoMajor, req.ProtoMinor))
		resp.Header().Set("x-test-req-url", req.URL.String())
		resp.Header().Set("x-test-req-host", req.Host)
		resp.Header().Set("x-test-backend-addr", serverAddr)
		resp.WriteHeader(http.StatusAccepted) // accepted to make sure stuff is slightly different.
	})
}

type localBackends struct {
	mu         sync.RWMutex
	resolvable int
	listeners  []net.Listener
	servers    []*http.Server
}

func buildAndStartServer(t *testing.T, config *tls.Config) (net.Listener, *http.Server) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "must be able to allocate a port for localBackend")
	if config != nil {
		listener = tls.NewListener(listener, config)
	}
	server := &http.Server{
		Handler: unknownPingbackHandler(listener.Addr().String()),
	}
	go func() {
		server.Serve(listener)
	}()
	return listener, server
}

func (l *localBackends) addServer(t *testing.T, config *tls.Config) {
	listener, server := buildAndStartServer(t, config)
	l.mu.Lock()
	l.servers = append(l.servers, server)
	l.listeners = append(l.listeners, listener)
	l.mu.Unlock()

}

func (l *localBackends) setResolvableCount(count int) {
	l.mu.Lock()
	l.resolvable = count
	l.mu.Unlock()
}

func (l *localBackends) targets() (targets []*srv.Target) {
	l.mu.RLock()
	for i := 0; i < l.resolvable && i < len(l.listeners); i++ {
		targets = append(targets, &srv.Target{
			Ttl:      backendResolutionDuration,
			DialAddr: l.listeners[i].Addr().String(),
		})
	}
	defer l.mu.RUnlock()
	return targets
}

func (l *localBackends) Close() error {
	for _, l := range l.listeners {
		l.Close()
	}
	return nil
}

type BackendPoolIntegrationTestSuite struct {
	suite.Suite

	proxy              *http.Server
	proxyListenerPlain net.Listener
	proxyListenerTls   net.Listener

	originalSrvResolver srv.Resolver
	originalAResolver   func(addr string) (names []string, err error)

	localBackends map[string]*localBackends
}

func TestBackendPoolIntegrationTestSuite(t *testing.T) {
	suite.Run(t, &BackendPoolIntegrationTestSuite{})
}

// implements srv resolver.
func (s *BackendPoolIntegrationTestSuite) Lookup(domainName string) ([]*srv.Target, error) {
	local, ok := s.localBackends[domainName]
	if !ok {
		return nil, fmt.Errorf("Unknown local backend '%v' in testing", domainName)
	}
	return local.targets(), nil
}

// implements A resolver that always resolves local host.
func (s *BackendPoolIntegrationTestSuite) LookupAddr(addr string) (names []string, err error) {
	return []string{"127.0.0.1"}, nil
}

func (s *BackendPoolIntegrationTestSuite) SetupSuite() {
	var err error
	s.proxyListenerPlain, err = net.Listen("tcp", "localhost:0")
	require.NoError(s.T(), err, "must be able to allocate a port for proxyListenerPlain")
	s.proxyListenerTls, err = net.Listen("tcp", "localhost:0")
	require.NoError(s.T(), err, "must be able to allocate a port for proxyListener")
	s.proxyListenerTls = tls.NewListener(s.proxyListenerTls, s.tlsConfigForTest())

	// Make ourselves the resolver for SRV for our backends. See Lookup function.
	s.originalSrvResolver = resolvers.ParentSrvResolver
	resolvers.ParentSrvResolver = s
	// Make ourselves the A resolver for backends for the Addresser.
	s.originalAResolver = router.DefaultALookup
	router.DefaultALookup = s.LookupAddr

	s.buildBackends()

	pool, err := backendpool.NewStatic(backendConfigs)
	require.NoError(s.T(), err, "backend pool creation must not fail")
	staticRouter := router.NewStatic(routeConfigs)
	addresser := router.NewAddresser(adhocConfig)
	s.proxy = &http.Server{
		Handler: director.New(pool, staticRouter, addresser),
	}

	go func() {
		s.proxy.Serve(s.proxyListenerPlain)
	}()
	go func() {
		s.proxy.Serve(s.proxyListenerTls)
	}()
}

func (s *BackendPoolIntegrationTestSuite) reverseProxyClient(listener net.Listener) *http.Client {
	proxyTlsClientConfig := s.tlsConfigForTest()
	proxyTlsClientConfig.InsecureSkipVerify = true // the proxy can be dialed over many different hostnames
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", listener.Addr().String())
			},
			TLSClientConfig: proxyTlsClientConfig,
		},
	}
}

func (s *BackendPoolIntegrationTestSuite) forwardProxyClient(listener net.Listener) *http.Client {
	client := s.reverseProxyClient(listener)
	// This will make all dials over the Proxy mechanism. For "http" schemes it will used FORWARD_PROXY semantics.
	// For "https" scheme it will use CONNECT proxy.
	(client.Transport).(*http.Transport).Proxy = func(req *http.Request) (*url.URL, error) {
		if listener == s.proxyListenerPlain {
			return urlMustParse("http://address_overwritten_in_dialer_anyway"), nil
		}
		return nil, errors.New("Golang proxy logic cannot use HTTPS connecitons to proxy. Saad.")
	}
	return client
}

func (s *BackendPoolIntegrationTestSuite) buildBackends() {
	s.localBackends = make(map[string]*localBackends)
	nonSecure := &localBackends{}
	for i := 0; i < nonSecureBackendCount; i++ {
		nonSecure.addServer(s.T(), nil /* notls */)
	}
	nonSecure.setResolvableCount(100)
	s.localBackends["_http._tcp.nonsecure.backends.test.local"] = nonSecure
	secure := &localBackends{}
	http2ServerTlsConfig, err := connhelpers.TlsConfigWithHttp2Enabled(s.tlsConfigForTest())
	if err != nil {
		s.FailNow("cannot configure the tls config for http2")
	}
	for i := 0; i < secureBackendCount; i++ {
		secure.addServer(s.T(), http2ServerTlsConfig)
	}
	secure.setResolvableCount(100)
	s.localBackends["_https._tcp.secure.backends.test.local"] = secure
}

func (s *BackendPoolIntegrationTestSuite) SimpleCtx() context.Context {
	ctx, _ := context.WithTimeout(context.TODO(), 2*time.Second)
	return ctx
}

func (s *BackendPoolIntegrationTestSuite) assertSuccessfulPingback(req *http.Request, resp *http.Response, err error) {
	require.NoError(s.T(), err, "no error on a call to a nonsecure reverse proxy addr")
	assert.Empty(s.T(), resp.Header.Get("x-kedge-error"))
	require.Equal(s.T(), http.StatusAccepted, resp.StatusCode)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-url"), req.URL.Path, "path seen on backend must match requested path")
	assert.Equal(s.T(), resp.Header.Get("x-test-req-host"), req.URL.Host, "host seen on backend must match requested host")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverForwardProxy_DialUsingAddresser() {
	// Pick a port of any non secure backend.
	addr := s.localBackends["_http._tcp.nonsecure.backends.test.local"].targets()[0].DialAddr
	port := addr[strings.LastIndex(addr, ":")+1:]
	req := &http.Request{Method: "GET", URL: urlMustParse(fmt.Sprintf("http://127-0-0-1.pods.test.local:%s/some/strict/path", port))}
	resp, err := s.forwardProxyClient(s.proxyListenerPlain).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "1.1", "non secure backends are dialed over HTTP/1.1")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverReverseProxy_ToNonSecure_OverPlain() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://nonsecure.ext.example.com/some/strict/path")}
	resp, err := s.reverseProxyClient(s.proxyListenerPlain).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "1.1", "non secure backends are dialed over HTTP/1.1")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverReverseProxy_ToSecure_OverPlain() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://secure.ext.example.com/some/strict/path")}
	resp, err := s.reverseProxyClient(s.proxyListenerPlain).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "2.0", "secure backends are dialed over HTTP2")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverReverseProxy_ToNonSecure_OverTls() {
	req := &http.Request{Method: "GET", URL: urlMustParse("https://nonsecure.ext.example.com/some/strict/path")}
	resp, err := s.reverseProxyClient(s.proxyListenerTls).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "1.1", "non secure backends are dialed over HTTP/1.1")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverReverseProxy_ToSecure_OverTls() {
	req := &http.Request{Method: "GET", URL: urlMustParse("https://secure.ext.example.com/some/strict/path")}
	resp, err := s.reverseProxyClient(s.proxyListenerTls).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "2.0", "secure backends are dialed over HTTP2")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverForwardProxy_ToNonSecure_OverPlain() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://nonsecure.backends.test.local/some/strict/path")}
	resp, err := s.forwardProxyClient(s.proxyListenerPlain).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "1.1", "non secure backends are dialed over HTTP/1.1")
}

func (s *BackendPoolIntegrationTestSuite) TestSuccessOverForwardProxy_ToSecure_OverPlain() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://secure.backends.test.local/some/strict/path")}
	resp, err := s.forwardProxyClient(s.proxyListenerPlain).Do(req)
	s.assertSuccessfulPingback(req, resp, err)
	assert.Equal(s.T(), resp.Header.Get("x-test-req-proto"), "2.0", "secure backends are dialed over HTTP2")
}

func (s *BackendPoolIntegrationTestSuite) TestFailOverReverseProxy_ToForwardSecure_OverPlain() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://secure.backends.test.local/some/strict/path")}
	resp, err := s.reverseProxyClient(s.proxyListenerPlain).Do(req)
	require.NoError(s.T(), err, "dialing should not fail")
	assert.Equal(s.T(), http.StatusBadGateway, resp.StatusCode, "routing should fail")
	assert.Equal(s.T(), "unknown route to service", resp.Header.Get("x-kedge-error"), "routing error should be in the header")
}

func (s *BackendPoolIntegrationTestSuite) TestFailOverForwardProxy_ToReverseNonSecure_OverPlain() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://nonsecure.ext.example.com/some/strict/path")}
	resp, err := s.forwardProxyClient(s.proxyListenerPlain).Do(req)
	require.NoError(s.T(), err, "dialing should not fail")
	assert.Equal(s.T(), http.StatusBadGateway, resp.StatusCode, "routing should fail")
	assert.Equal(s.T(), "unknown route to service", resp.Header.Get("x-kedge-error"), "routing error should be in the header")
}

func (s *BackendPoolIntegrationTestSuite) TestFailOverReverseProxy_NonSecureWithBadPath() {
	req := &http.Request{Method: "GET", URL: urlMustParse("http://nonsecure.ext.example.com/other_path")}
	resp, err := s.reverseProxyClient(s.proxyListenerPlain).Do(req)
	require.NoError(s.T(), err, "dialing should not fail")
	assert.Equal(s.T(), http.StatusBadGateway, resp.StatusCode, "routing should fail")
	assert.Equal(s.T(), "unknown route to service", resp.Header.Get("x-kedge-error"), "routing error should be in the header")
}

func (s *BackendPoolIntegrationTestSuite) TestLoadbalacingToSecureBackend() {
	backendResponse := make(map[string]int)
	for i := 0; i < secureBackendCount*10; i++ {
		req := &http.Request{Method: "GET", URL: urlMustParse("http://secure.backends.test.local/some/strict/path")}
		resp, err := s.forwardProxyClient(s.proxyListenerPlain).Do(req)
		s.assertSuccessfulPingback(req, resp, err)
		addr := resp.Header.Get("x-test-backend-addr")
		if _, ok := backendResponse[addr]; ok {
			backendResponse[addr] += 1
		} else {
			backendResponse[addr] = 1
		}
	}
	assert.Len(s.T(), backendResponse, secureBackendCount, "requests should hit all backends")
	for addr, value := range backendResponse {
		assert.Equal(s.T(), 10, value, "backend %v should have received the same amount of requests", addr)
	}
}

func (s *BackendPoolIntegrationTestSuite) TestLoadbalacingToNonSecureBackend() {
	backendResponse := make(map[string]int)
	for i := 0; i < nonSecureBackendCount*10; i++ {
		req := &http.Request{Method: "GET", URL: urlMustParse("http://nonsecure.ext.example.com/some/strict/path")}
		resp, err := s.reverseProxyClient(s.proxyListenerPlain).Do(req)
		s.assertSuccessfulPingback(req, resp, err)
		addr := resp.Header.Get("x-test-backend-addr")
		if _, ok := backendResponse[addr]; ok {
			backendResponse[addr] += 1
		} else {
			backendResponse[addr] = 1
		}
	}
	assert.Len(s.T(), backendResponse, nonSecureBackendCount, "requests should hit all backends")
	for addr, value := range backendResponse {
		assert.Equal(s.T(), 10, value, "backend %v should have received the same amount of requests", addr)
	}
}

//
//func (s *BackendPoolIntegrationTestSuite) TestCallOverForwardProxy_Tls() {
//	req := &http.Request{Method: "GET", URL: urlMustParse("http://nonsecure.ext.example.com/some/strict/path")}
//	resp, err := s.forwardProxyClient(s.proxyListenerTls).Do(req)
//	s.assertSuccessfulPingback(resp, err)
//}

//

//
//func (s *BackendPoolIntegrationTestSuite) TestCallToUnknownRouteCausesError() {
//	err := grpc.Invoke(s.SimpleCtx(), "/bad.route.doesnt.exist/Method", &pb_testproto.Empty{}, &pb_testproto.Empty{}, s.proxyConn)
//	require.EqualError(s.T(), err, "rpc error: code = 12 desc = unknown route to service", "no error on simple call")
//}
//
//func (s *BackendPoolIntegrationTestSuite) TestCallToUnknownBackend() {
//	err := grpc.Invoke(s.SimpleCtx(), "/bad.backend.doesnt.exist/Method", &pb_testproto.Empty{}, &pb_testproto.Empty{}, s.proxyConn)
//	require.EqualError(s.T(), err, "rpc error: code = 12 desc = unknown backend", "no error on simple call")
//}

func (s *BackendPoolIntegrationTestSuite) TearDownSuite() {
	// Restore old resolver.
	if s.originalSrvResolver != nil {
		resolvers.ParentSrvResolver = s.originalSrvResolver
	}
	if s.originalAResolver != nil {
		router.DefaultALookup = s.originalAResolver
	}
	time.Sleep(10 * time.Millisecond)
	if s.proxy != nil {
		s.proxyListenerTls.Close()
		s.proxyListenerPlain.Close()
	}
	for _, be := range s.localBackends {
		be.Close()
	}
}

func (s *BackendPoolIntegrationTestSuite) tlsConfigForTest() *tls.Config {
	tlsConfig, err := connhelpers.TlsConfigForServerCerts(
		path.Join(getTestingCertsPath(), "localhost.crt"),
		path.Join(getTestingCertsPath(), "localhost.key"))
	if err != nil {
		require.NoError(s.T(), err, "failed reading server certs")
	}
	tlsConfig.RootCAs = x509.NewCertPool()
	// Make Client cert verification an option.
	tlsConfig.ClientCAs = x509.NewCertPool()
	tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
	data, err := ioutil.ReadFile(path.Join(getTestingCertsPath(), "ca.crt"))
	if err != nil {
		s.FailNow("Failed reading CA: %v", err)
	}
	if ok := tlsConfig.RootCAs.AppendCertsFromPEM(data); !ok {
		s.FailNow("failed processing CA file")
	}
	if ok := tlsConfig.ClientCAs.AppendCertsFromPEM(data); !ok {
		s.FailNow("failed processing CA file")
	}
	return tlsConfig
}

func getTestingCertsPath() string {
	_, callerPath, _, _ := runtime.Caller(0)
	return path.Join(path.Dir(callerPath), "..", "misc")
}

func urlMustParse(uStr string) *url.URL {
	u, err := url.Parse(uStr)
	if err != nil {
		panic(err)
	}
	return u
}
