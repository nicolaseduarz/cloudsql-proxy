package grpcproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/cloudsql-proxy/logging"
	pb "github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/grpcproxy/proto"
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/proxy"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// RPCSQLProxyConnection wraps around Mygrpc_ConnectionClient and can create a tunnel between a local connection and its remote RPC server
type RPCSQLProxyConnection struct {
	remote pb.MyGrpcClient
}

// Run causes the client to start waiting for new connections to connSrc and
// proxy them to the destination instance. It blocks until connSrc is closed.
func Run(connSrc <-chan proxy.Conn, grpcPort int, client *proxy.Client) {
	for conn := range connSrc {
		rpcProxy, err := ObtainProxyConnection(AuthConfig{
			ProxyClient: client,
			Conn:        conn,
			Port:        grpcPort,
		})
		if err != nil {
			logging.Errorf("Failed to connect to gRPC service: %v", err)
		}

		err = rpcProxy.CreateTunnel(conn.Conn)
		if err != nil {
			logging.Errorf("Failed to create gRPC tunnel: %v", err)
		}
	}
}

// CreateTunnel establishes a tunnel between remote and the local stream
func (conn *RPCSQLProxyConnection) CreateTunnel(local io.ReadWriteCloser) error {
	ctx, cancel := context.WithCancel(context.Background())

	forward, err := conn.remote.Connection(ctx)

	if err != nil {
		cancel()
		return err
	}

	go copyThenClose(forward, local, cancel)
	return nil
}

// ObtainProxyConnection returns an object that can be used to make RPC calls to the proxy server
func ObtainProxyConnection(conf AuthConfig) (RPCSQLProxyConnection, error) {
	c := conf.ProxyClient
	instance := conf.Conn.Instance

	var cfg *tls.Config
	var err error
	var addr string
	if addr, cfg = c.CachedCfg(instance); cfg == nil {
		addr, cfg, err = c.RefreshCfg(instance)
		if err != nil {
			return RPCSQLProxyConnection{}, err
		}
	}
	addr = addr[:strings.Index(addr, ":")]
	addr = fmt.Sprintf("%s:%d", addr, conf.Port)

	// remove region name from instance
	indexOfColon := strings.Index(instance, ":")
	secondPortion := instance[indexOfColon+1:]
	cfg.ServerName = instance[0:indexOfColon] + secondPortion[strings.Index(secondPortion, ":"):]

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(
			credentials.NewTLS(cfg),
		),
	}

	conn, err := grpc.Dial(addr, opts...)
	if err != nil {
		return RPCSQLProxyConnection{}, err
	}

	remote := pb.NewMyGrpcClient(conn)

	return RPCSQLProxyConnection{remote: remote}, nil
}

// AuthConfig represents the command-line arguments needed to authenticate
type AuthConfig struct {
	ProxyClient *proxy.Client
	Port        int
	Conn        proxy.Conn
}

func copyThenClose(remote pb.MyGrpc_ConnectionClient, local io.ReadWriteCloser, cancel context.CancelFunc) {
	defer cancel()

	firstErr := make(chan error, 1)

	go func() {
		readErr, err := copyBytesFromRPC(remote, local, 1024)
		select {
		case firstErr <- err:
			logging.Infof("Error %v", err)
			if readErr && err == io.EOF {
				// logging.Verbosef("Client closed %v", localDesc)
			} else {
				// copyError(localDesc, remoteDesc, readErr, err)
			}
			// remote.Close()
			cancel()
			local.Close()
		default:
		}
	}()

	readErr, err := copyBytesToRPC(local, remote, 1024)
	select {
	case firstErr <- err:
		if readErr && err == io.EOF {
			// logging.Verbosef("Instance %v closed connection", remoteDesc)
		} else {
			// copyError(remoteDesc, localDesc, readErr, err)
		}
		// remote.Close()
		cancel()
		local.Close()
	default:
		// In this case, the other goroutine exited first and already printed its
		// error (and closed the things).
	}
	logging.Infof("Closing connection")
}

func copyBytesToRPC(server io.ReadWriteCloser, client pb.MyGrpc_ConnectionClient, bufferSize int) (readErr bool, err error) {
	buf := make([]byte, bufferSize)
	for {
		len, err := server.Read(buf)
		if len > 0 {
			if err != nil {
				return true, err
			}

			err = client.Send(&pb.ClientMessage{Data: buf[:len]})
			if err != nil {
				return false, err
			}
		}
	}

}

func copyBytesFromRPC(client pb.MyGrpc_ConnectionClient, server io.ReadWriteCloser, bufferSize int) (readErr bool, err error) {
	for {
		msg, err := client.Recv()
		if err != nil {
			return true, err
		}
		_, err = server.Write(msg.Data)

		if err != nil {
			return false, err
		}

	}

}