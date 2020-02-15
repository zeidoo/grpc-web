package proxybuilder

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	"github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/mwitkow/go-conntrack"
	"github.com/mwitkow/go-conntrack/connhelpers"
	"github.com/mwitkow/grpc-proxy/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	BindAddress                     string
	HttpPort                        int
	HttpTlsPort                     int
	AllowAllOrigins                 bool
	AllowedOrigins                  []string
	AllowedHeaders                  []string
	RunHttpServer                   bool
	RunTlsServer                    bool
	UseWebSockets                   bool
	WebSocketPingInterval           time.Duration
	HttpMaxWriteTimeout             time.Duration
	HttpMaxReadTimeout              time.Duration
	BackendHostPort                 string
	BackendIsUsingTls               bool
	BackendTlsNoVerify              bool
	BackendTlsClientCert            string
	BackendTlsClientKey             string
	BackendMaxCallRecvMsgSize       int
	BackendTlsCa                    []string
	BackendDefaultAuthority         string
	BackendBackoffMaxDelay          time.Duration
	TlsServerCert                   string
	TlsServerKey                    string
	TlsServerClientCertVerification string
	TlsServerClientCAFiles          []string
}

var DefaultConfig = &Config{
	BindAddress:                     "0.0.0.0",
	HttpPort:                        8080,
	HttpTlsPort:                     8443,
	AllowAllOrigins:                 false,
	AllowedOrigins:                  nil,
	AllowedHeaders:                  []string{},
	UseWebSockets:                   false,
	WebSocketPingInterval:           0,
	HttpMaxWriteTimeout:             10 * time.Second,
	HttpMaxReadTimeout:              10 * time.Second,
	BackendHostPort:                 "",
	BackendIsUsingTls:               false,
	BackendTlsNoVerify:              false,
	BackendTlsClientCert:            "",
	BackendTlsClientKey:             "",
	BackendMaxCallRecvMsgSize:       1024 * 1024 * 4,
	BackendTlsCa:                    []string{},
	BackendDefaultAuthority:         "",
	BackendBackoffMaxDelay:          grpc.DefaultBackoffConfig.MaxDelay,
	TlsServerCert:                   "",
	TlsServerKey:                    "../misc/localhost.key",
	TlsServerClientCertVerification: "none",
	TlsServerClientCAFiles:          []string{},
}

type ProxyBuilder struct {
	Config *Config
}

func (pb *ProxyBuilder) GetHttpGrpcServer(logEntry *logrus.Entry) *http.Server {
	grpcServer := pb.buildGrpcProxyServer(logEntry)
	allowedOrigins := pb.makeAllowedOrigins(pb.Config.AllowedOrigins)

	options := []grpcweb.Option{
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
		grpcweb.WithOriginFunc(pb.makeHttpOriginFunc(allowedOrigins)),
	}

	if pb.Config.UseWebSockets {
		logrus.Println("using websockets")
		options = append(
			options,
			grpcweb.WithWebsockets(true),
			grpcweb.WithWebsocketOriginFunc(pb.makeWebsocketOriginFunc(allowedOrigins)),
		)
		if pb.Config.WebSocketPingInterval >= time.Second {
			logrus.Infof("websocket keepalive pinging enabled, the timeout interval is %s", pb.Config.WebSocketPingInterval.String())
		}
		options = append(
			options,
			grpcweb.WithWebsocketPingInterval(pb.Config.WebSocketPingInterval),
		)
	}

	if len(pb.Config.AllowedHeaders) > 0 {
		options = append(
			options,
			grpcweb.WithAllowedRequestHeaders(pb.Config.AllowedHeaders),
		)
	}

	wrappedGrpc := grpcweb.WrapServer(grpcServer, options...)
	return pb.buildServer(wrappedGrpc)
}
func (pb *ProxyBuilder) ConfigureHttpServer(server *http.Server) (*http.Server, net.Listener) {
	http.Handle("/metrics", promhttp.Handler())
	listener := pb.buildListenerOrFail("http", pb.Config.HttpPort)
	return server, listener
}
func (pb *ProxyBuilder) ConfigureTlsServer(server *http.Server) (*http.Server, net.Listener) {
	listener := pb.buildListenerOrFail("http", pb.Config.HttpTlsPort)
	listener = tls.NewListener(listener, pb.buildServerTlsOrFail())
	return server, listener
}
func (pb *ProxyBuilder) buildServer(wrappedGrpc *grpcweb.WrappedGrpcServer) *http.Server {
	return &http.Server{
		WriteTimeout: pb.Config.HttpMaxWriteTimeout,
		ReadTimeout:  pb.Config.HttpMaxReadTimeout,
		Handler: http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			if strings.HasPrefix(req.Header.Get("Content-Type"), "application/grpc-web-text") {
				wrappedGrpc.ServeHTTP(resp, req)
			} else {
				http.DefaultServeMux.ServeHTTP(resp, req)
			}
		}),
	}
}
func (pb *ProxyBuilder) buildGrpcProxyServer(logger *logrus.Entry) *grpc.Server {
	// gRPC-wide changes.
	grpc.EnableTracing = true
	grpc_logrus.ReplaceGrpcLogger(logger)

	// gRPC proxy logic.
	backendConn := pb.dialBackendOrFail()
	director := func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		outCtx, _ := context.WithCancel(ctx)
		mdCopy := md.Copy()
		delete(mdCopy, "user-agent")
		// If this header is present in the request from the web client,
		// the actual connection to the backend will not be established.
		// https://github.com/improbable-eng/grpc-web/issues/568
		delete(mdCopy, "connection")
		outCtx = metadata.NewOutgoingContext(outCtx, mdCopy)
		return outCtx, backendConn, nil
	}
	// Server with logging and monitoring enabled.
	return grpc.NewServer(
		grpc.CustomCodec(proxy.Codec()), // needed for proxy to function.
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
		grpc_middleware.WithUnaryServerChain(
			grpc_logrus.UnaryServerInterceptor(logger),
			grpc_prometheus.UnaryServerInterceptor,
		),
		grpc_middleware.WithStreamServerChain(
			grpc_logrus.StreamServerInterceptor(logger),
			grpc_prometheus.StreamServerInterceptor,
		),
	)
}
func (pb *ProxyBuilder) buildListenerOrFail(name string, port int) net.Listener {
	addr := fmt.Sprintf("%s:%d", pb.Config.BindAddress, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed listening for '%v' on %v: %v", name, port, err)
	}
	return conntrack.NewListener(listener,
		conntrack.TrackWithName(name),
		conntrack.TrackWithTcpKeepAlive(20*time.Second),
		conntrack.TrackWithTracing(),
	)
}
func (pb *ProxyBuilder) makeHttpOriginFunc(allowedOrigins *allowedOrigins) func(origin string) bool {
	if pb.Config.AllowAllOrigins {
		return func(origin string) bool {
			return true
		}
	}
	return allowedOrigins.IsAllowed
}
func (pb *ProxyBuilder) makeWebsocketOriginFunc(allowedOrigins *allowedOrigins) func(req *http.Request) bool {
	if pb.Config.AllowAllOrigins {
		return func(req *http.Request) bool {
			return true
		}
	} else {
		return func(req *http.Request) bool {
			origin, err := grpcweb.WebsocketRequestOrigin(req)
			if err != nil {
				grpclog.Warning(err)
				return false
			}
			return allowedOrigins.IsAllowed(origin)
		}
	}
}
func (pb *ProxyBuilder) makeAllowedOrigins(origins []string) *allowedOrigins {
	o := map[string]struct{}{}
	for _, allowedOrigin := range origins {
		o[allowedOrigin] = struct{}{}
	}
	return &allowedOrigins{
		origins: o,
	}
}
func (pb *ProxyBuilder) dialBackendOrFail() *grpc.ClientConn {
	if pb.Config.BackendHostPort == "" {
		logrus.Fatalf("flag 'backend_addr' must be set")
	}
	opt := []grpc.DialOption{}
	opt = append(opt, grpc.WithCodec(proxy.Codec()))

	if pb.Config.BackendDefaultAuthority != "" {
		opt = append(opt, grpc.WithAuthority(pb.Config.BackendDefaultAuthority))
	}

	if pb.Config.BackendIsUsingTls {
		opt = append(opt, grpc.WithTransportCredentials(credentials.NewTLS(pb.buildBackendTlsOrFail())))
	} else {
		opt = append(opt, grpc.WithInsecure())
	}

	opt = append(opt,
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(pb.Config.BackendMaxCallRecvMsgSize)),
		grpc.WithBackoffMaxDelay(pb.Config.BackendBackoffMaxDelay),
	)

	cc, err := grpc.Dial(pb.Config.BackendHostPort, opt...)
	if err != nil {
		logrus.Fatalf("failed dialing backend: %v", err)
	}
	return cc
}
func (pb *ProxyBuilder) buildBackendTlsOrFail() *tls.Config {
	tlsConfig := &tls.Config{}
	tlsConfig.MinVersion = tls.VersionTLS12
	if pb.Config.BackendTlsNoVerify {
		tlsConfig.InsecureSkipVerify = true
	} else {
		if len(pb.Config.BackendTlsCa) > 0 {
			tlsConfig.RootCAs = x509.NewCertPool()
			for _, path := range pb.Config.BackendTlsCa {
				data, err := ioutil.ReadFile(path)
				if err != nil {
					logrus.Fatalf("failed reading backend CA file %v: %v", path, err)
				}
				if ok := tlsConfig.RootCAs.AppendCertsFromPEM(data); !ok {
					logrus.Fatalf("failed processing backend CA file %v", path)
				}
			}
		}
	}
	if pb.Config.BackendTlsClientCert != "" || pb.Config.BackendTlsClientKey != "" {
		if pb.Config.BackendTlsClientCert == "" {
			logrus.Fatal("flag 'backend_client_tls_cert_file' must be set when 'backend_client_tls_key_file' is set")
		}
		if pb.Config.BackendTlsClientKey == "" {
			logrus.Fatal("flag 'backend_client_tls_key_file' must be set when 'backend_client_tls_cert_file' is set")
		}
		cert, err := tls.LoadX509KeyPair(pb.Config.BackendTlsClientCert, pb.Config.BackendTlsClientKey)
		if err != nil {
			logrus.Fatalf("failed reading TLS client keys: %v", err)
		}
		tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
	}
	return tlsConfig
}
func (pb *ProxyBuilder) buildServerTlsOrFail() *tls.Config {
	if pb.Config.TlsServerCert == "" || pb.Config.TlsServerKey == "" {
		logrus.Fatalf("flags server_tls_cert_file and server_tls_key_file must be set")
	}
	tlsConfig, err := connhelpers.TlsConfigForServerCerts(pb.Config.TlsServerCert, pb.Config.TlsServerKey)
	if err != nil {
		logrus.Fatalf("failed reading TLS server keys: %v", err)
	}
	tlsConfig.MinVersion = tls.VersionTLS12
	switch pb.Config.TlsServerClientCertVerification {
	case "none":
		tlsConfig.ClientAuth = tls.NoClientCert
	case "verify_if_given":
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
	case "require":
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		logrus.Fatalf("Uknown value '%v' for server_tls_client_cert_verification", pb.Config.TlsServerClientCertVerification)
	}
	if tlsConfig.ClientAuth != tls.NoClientCert {
		if len(pb.Config.TlsServerClientCAFiles) > 0 {
			tlsConfig.ClientCAs = x509.NewCertPool()
			for _, path := range pb.Config.TlsServerClientCAFiles {
				data, err := ioutil.ReadFile(path)
				if err != nil {
					logrus.Fatalf("failed reading client CA file %v: %v", path, err)
				}
				if ok := tlsConfig.ClientCAs.AppendCertsFromPEM(data); !ok {
					logrus.Fatalf("failed processing client CA file %v", path)
				}
			}
		} else {
			var err error
			tlsConfig.ClientCAs, err = x509.SystemCertPool()
			if err != nil {
				logrus.Fatalf("no client CA files specified, fallback to system CA chain failed: %v", err)
			}
		}

	}
	tlsConfig, err = connhelpers.TlsConfigWithHttp2Enabled(tlsConfig)
	if err != nil {
		logrus.Fatalf("can't configure h2 handling: %v", err)
	}
	return tlsConfig
}

type allowedOrigins struct {
	origins map[string]struct{}
}

func (a *allowedOrigins) IsAllowed(origin string) bool {
	_, ok := a.origins[origin]
	return ok
}
