package main

import (
	"fmt"
	"google.golang.org/grpc"
	"net"
	"net/http"
	_ "net/http/pprof" // register in DefaultServerMux
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	_ "golang.org/x/net/trace" // register in DefaultServerMux
)

var (
	flagBindAddr    = pflag.String("server_bind_address", "0.0.0.0", "address to bind the server to")
	flagHttpPort    = pflag.Int("server_http_debug_port", 8080, "TCP port to listen on for HTTP1.1 debug calls.")
	flagHttpTlsPort = pflag.Int("server_http_tls_port", 8443, "TCP port to listen on for HTTPS (gRPC, gRPC-Web).")

	flagAllowAllOrigins = pflag.Bool("allow_all_origins", false, "allow requests from any origin.")
	flagAllowedOrigins  = pflag.StringSlice("allowed_origins", nil, "comma-separated list of origin URLs which are allowed to make cross-origin requests.")
	flagAllowedHeaders  = pflag.StringSlice("allowed_headers", []string{}, "comma-separated list of headers which are allowed to propagate to the gRPC backend.")

	runHttpServer = pflag.Bool("run_http_server", true, "whether to run HTTP server")
	runTlsServer  = pflag.Bool("run_tls_server", true, "whether to run TLS server")

	useWebsockets         = pflag.Bool("use_websockets", false, "whether to use beta websocket transport layer")
	websocketPingInterval = pflag.Duration("websocket_ping_interval", 0, "whether to use websocket keepalive pinging. Only used when using websockets. Configured interval must be >= 1s.")

	flagHttpMaxWriteTimeout = pflag.Duration("server_http_max_write_timeout", 10*time.Second, "HTTP server config, max write duration.")
	flagHttpMaxReadTimeout  = pflag.Duration("server_http_max_read_timeout", 10*time.Second, "HTTP server config, max read duration.")

	flagBackendHostPort      = pflag.String("backend_addr", "", "A host:port (IP or hostname) of the gRPC server to forward it to.")
	flagBackendIsUsingTls    = pflag.Bool("backend_tls", false, "Whether the gRPC server of the backend is serving in plaintext (false) or over TLS (true).")
	flagBackendTlsNoVerify   = pflag.Bool("backend_tls_noverify", false, "Whether to ignore TLS verification checks (cert validity, hostname). *DO NOT USE IN PRODUCTION*.")
	flagBackendTlsClientCert = pflag.String("backend_client_tls_cert_file", "", "Path to the PEM certificate used when the backend requires client certificates for TLS.")
	flagBackendTlsClientKey  = pflag.String("backend_client_tls_key_file", "", "Path to the PEM key used when the backend requires client certificates for TLS.")
	// The current maximum receive msg size per https://github.com/grpc/grpc-go/blob/v1.8.2/server.go#L54
	flagMaxCallRecvMsgSize      = pflag.Int("backend_max_call_recv_msg_size", 1024*1024*4, "Maximum receive message size limit. If not specified, the default of 4MB will be used.")
	flagBackendTlsCa            = pflag.StringSlice("backend_tls_ca_files", []string{}, "Paths (comma separated) to PEM certificate chains used for verification of backend certificates. If empty, host CA chain will be used.")
	flagBackendDefaultAuthority = pflag.String("backend_default_authority", "", "Default value to use for the HTTP/2 :authority header commonly used for routing gRPC calls through a backend gateway.")
	flagBackendBackoffMaxDelay  = pflag.Duration("backend_backoff_max_delay", grpc.DefaultBackoffConfig.MaxDelay, "Maximum delay when backing off after failed connection attempts to the backend.")

	TlsServerCert                   = pflag.String("server_tls_cert_file", "", "Path to the PEM certificate for server use.")
	TlsServerKey                    = pflag.String("server_tls_key_file", "../misc/localhost.key", "Path to the PEM key for the certificate for the server use.")
	TlsServerClientCertVerification = pflag.String("server_tls_client_cert_verification", "none", "Controls whether a client certificate is on. Values: none, verify_if_given, require.")
	TlsServerClientCAFiles          = pflag.StringSlice("server_tls_client_ca_files", []string{}, "Paths (comma separated) to PEM certificate chains used for client-side verification. If empty, host CA chain will be used.")
)

func main() {
	pflag.Parse()
	for _, flag := range pflag.Args() {
		if flag == "true" || flag == "false" {
			logrus.Fatal("Boolean flags should be set using --flag=false, --flag=true or --flag (which is short for --flag=true). You cannot use --flag true or --flag false.")
		}
		logrus.Fatal("Unknown argument: " + flag)
	}

	logrus.SetOutput(os.Stdout)
	logEntry := logrus.NewEntry(logrus.StandardLogger())

	if *flagAllowAllOrigins && len(*flagAllowedOrigins) != 0 {
		logrus.Fatal("Ambiguous --allow_all_origins and --allow_origins configuration. Either set --allow_all_origins=true OR specify one or more origins to whitelist with --allow_origins, not both.")
	}
	if !*runHttpServer && !*runTlsServer {
		logrus.Fatalf("Both run_http_server and run_tls_server are set to false. At least one must be enabled for grpcweb p to function correctly.")
	}

	config := &Config{
		BindAddress:                     *flagBindAddr,
		HttpPort:                        *flagHttpPort,
		HttpTlsPort:                     *flagHttpTlsPort,
		AllowAllOrigins:                 *flagAllowAllOrigins,
		AllowedOrigins:                  *flagAllowedOrigins,
		AllowedHeaders:                  *flagAllowedHeaders,
		UseWebSockets:                   *useWebsockets,
		WebSocketPingInterval:           *websocketPingInterval,
		HttpMaxWriteTimeout:             *flagHttpMaxWriteTimeout,
		HttpMaxReadTimeout:              *flagHttpMaxReadTimeout,
		BackendHostPort:                 *flagBackendHostPort,
		BackendIsUsingTls:               *flagBackendIsUsingTls,
		BackendTlsNoVerify:              *flagBackendTlsNoVerify,
		BackendTlsClientCert:            *flagBackendTlsClientCert,
		BackendTlsClientKey:             *flagBackendTlsClientKey,
		BackendMaxCallRecvMsgSize:       *flagMaxCallRecvMsgSize,
		BackendTlsCa:                    *flagBackendTlsCa,
		BackendDefaultAuthority:         *flagBackendDefaultAuthority,
		BackendBackoffMaxDelay:          *flagBackendBackoffMaxDelay,
		TlsServerCert:                   *TlsServerCert,
		TlsServerKey:                    *TlsServerKey,
		TlsServerClientCertVerification: *TlsServerClientCertVerification,
		TlsServerClientCAFiles:          *TlsServerClientCAFiles,
	}

	proxy := &ProxyBuilder{Config: config}
	errChan := make(chan error)

	httpServer := proxy.GetHttpGrpcServer(logEntry)
	if *runHttpServer {
		srv, lis := proxy.ConfigureHttpServer(httpServer)
		ServeServer(srv, lis, "http", errChan)
	}

	if *runTlsServer {
		srv, lis := proxy.ConfigureTlsServer(httpServer)
		ServeServer(srv, lis, "http_tls", errChan)
	}

	<-errChan
	// TODO(mwitkow): Add graceful shutdown.
}

func ServeServer(server *http.Server, listener net.Listener, name string, errChan chan error) {
	go func() {
		logrus.Infof("listening for %s on: %v", name, listener.Addr().String())
		if err := server.Serve(listener); err != nil {
			errChan <- fmt.Errorf("%s server error: %v", name, err)
		}
	}()
}
