package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"net"
	"time"

	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client/connhelper"
	"github.com/moby/buildkit/util/appdefaults"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Client struct {
	conn *grpc.ClientConn
}

type ClientOpt interface{}

// New returns a new buildkit client. Address can be empty for the system-default address.
func New(ctx context.Context, address string, opts ...ClientOpt) (*Client, error) {
	gopts := []grpc.DialOption{}
	needDialer := true
	needWithInsecure := true
	for _, o := range opts {
		if _, ok := o.(*withFailFast); ok {
			gopts = append(gopts, grpc.FailOnNonTempDialError(true))
		}
		if credInfo, ok := o.(*withCredentials); ok {
			opt, err := loadCredentials(credInfo)
			if err != nil {
				return nil, err
			}
			gopts = append(gopts, opt)
			needWithInsecure = false
		}
		if wt, ok := o.(*withTracer); ok {
			gopts = append(gopts,
				grpc.WithUnaryInterceptor(otgrpc.OpenTracingClientInterceptor(wt.tracer, otgrpc.LogPayloads())),
				grpc.WithStreamInterceptor(otgrpc.OpenTracingStreamClientInterceptor(wt.tracer)))
		}
		if wd, ok := o.(*withDialer); ok {
			gopts = append(gopts, grpc.WithDialer(wd.dialer))
			needDialer = false
		}
	}
	if needDialer {
		dialFn, err := resolveDialer(address)
		if err != nil {
			return nil, err
		}
		// TODO(AkihiroSuda): use WithContextDialer (requires grpc 1.19)
		// https://github.com/grpc/grpc-go/commit/40cb5618f475e7b9d61aa7920ae4b04ef9bbaf89
		gopts = append(gopts, grpc.WithDialer(dialFn))
	}
	if needWithInsecure {
		gopts = append(gopts, grpc.WithInsecure())
	}
	if address == "" {
		address = appdefaults.Address
	}
	conn, err := grpc.DialContext(ctx, address, gopts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial %q . make sure buildkitd is running", address)
	}
	c := &Client{
		conn: conn,
	}
	return c, nil
}

func (c *Client) controlClient() controlapi.ControlClient {
	return controlapi.NewControlClient(c.conn)
}

func (c *Client) Close() error {
	return c.conn.Close()
}

type withFailFast struct{}

func WithFailFast() ClientOpt {
	return &withFailFast{}
}

type withDialer struct {
	dialer func(string, time.Duration) (net.Conn, error)
}

func WithDialer(df func(string, time.Duration) (net.Conn, error)) ClientOpt {
	return &withDialer{dialer: df}
}

type withCredentials struct {
	ServerName string
	CACert     string
	Cert       string
	Key        string
}

// WithCredentials configures the TLS parameters of the client.
// Arguments:
// * serverName: specifies the name of the target server
// * ca:				 specifies the filepath of the CA certificate to use for verification
// * cert:			 specifies the filepath of the client certificate
// * key:				 specifies the filepath of the client key
func WithCredentials(serverName, ca, cert, key string) ClientOpt {
	return &withCredentials{serverName, ca, cert, key}
}

func loadCredentials(opts *withCredentials) (grpc.DialOption, error) {
	ca, err := ioutil.ReadFile(opts.CACert)
	if err != nil {
		return nil, errors.Wrap(err, "could not read ca certificate")
	}

	certPool := x509.NewCertPool()
	if ok := certPool.AppendCertsFromPEM(ca); !ok {
		return nil, errors.New("failed to append ca certs")
	}

	cfg := &tls.Config{
		ServerName: opts.ServerName,
		RootCAs:    certPool,
	}

	// we will produce an error if the user forgot about either cert or key if at least one is specified
	if opts.Cert != "" || opts.Key != "" {
		cert, err := tls.LoadX509KeyPair(opts.Cert, opts.Key)
		if err != nil {
			return nil, errors.Wrap(err, "could not read certificate/key")
		}
		cfg.Certificates = []tls.Certificate{cert}
		cfg.BuildNameToCertificate()
	}

	return grpc.WithTransportCredentials(credentials.NewTLS(cfg)), nil
}

func WithTracer(t opentracing.Tracer) ClientOpt {
	return &withTracer{t}
}

type withTracer struct {
	tracer opentracing.Tracer
}

func resolveDialer(address string) (func(string, time.Duration) (net.Conn, error), error) {
	ch, err := connhelper.GetConnectionHelper(address)
	if err != nil {
		return nil, err
	}
	if ch != nil {
		f := func(a string, _ time.Duration) (net.Conn, error) {
			ctx := context.Background()
			return ch.ContextDialer(ctx, a)
		}
		return f, nil
	}
	// basic dialer
	return dialer, nil
}
