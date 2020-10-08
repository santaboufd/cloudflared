package connection

import (
	"context"
	"fmt"
	"io"

	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/tunnelrpc"
	tunnelpogs "github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"zombiezen.com/go/capnproto2/rpc"
)

type tunnelServerClient struct {
	client    tunnelpogs.TunnelServer_PogsClient
	transport rpc.Transport
}

// NewTunnelRPCClient creates and returns a new RPC client, which will communicate using a stream on the given muxer.
// This method is exported for supervisor to call Authenticate RPC
func NewTunnelServerClient(
	ctx context.Context,
	stream io.ReadWriteCloser,
	logger logger.Service,
) *tunnelServerClient {
	transport := tunnelrpc.NewTransportLogger(logger, rpc.StreamTransport(stream))
	conn := rpc.NewConn(
		transport,
		tunnelrpc.ConnLog(logger),
	)
	registrationClient := tunnelpogs.RegistrationServer_PogsClient{Client: conn.Bootstrap(ctx), Conn: conn}
	return &tunnelServerClient{
		client:    tunnelpogs.TunnelServer_PogsClient{RegistrationServer_PogsClient: registrationClient, Client: conn.Bootstrap(ctx), Conn: conn},
		transport: transport,
	}
}

func (tsc *tunnelServerClient) Authenticate(ctx context.Context, classicTunnel *ClassicTunnelConfig, registrationOptions *tunnelpogs.RegistrationOptions) (tunnelpogs.AuthOutcome, error) {
	authResp, err := tsc.client.Authenticate(ctx, classicTunnel.OriginCert, classicTunnel.Hostname, registrationOptions)
	if err != nil {
		return nil, err
	}
	return authResp.Outcome(), nil
}

func (tsc *tunnelServerClient) Close() {
	// Closing the client will also close the connection
	tsc.client.Close()
	tsc.transport.Close()
}

type registrationServerClient struct {
	client    tunnelpogs.RegistrationServer_PogsClient
	transport rpc.Transport
}

func newRegistrationRPCClient(
	ctx context.Context,
	stream io.ReadWriteCloser,
	logger logger.Service,
) *registrationServerClient {
	transport := tunnelrpc.NewTransportLogger(logger, rpc.StreamTransport(stream))
	conn := rpc.NewConn(
		transport,
		tunnelrpc.ConnLog(logger),
	)
	return &registrationServerClient{
		client:    tunnelpogs.RegistrationServer_PogsClient{Client: conn.Bootstrap(ctx), Conn: conn},
		transport: transport,
	}
}

func (rsc *registrationServerClient) close() {
	// Closing the client will also close the connection
	rsc.client.Close()
	// Closing the transport also closes the stream
	rsc.transport.Close()
}

type rpcName string

const (
	register     rpcName = "register"
	reconnect    rpcName = "reconnect"
	unregister   rpcName = "unregister"
	authenticate rpcName = " authenticate"
)

func registerConnection(
	ctx context.Context,
	rpcClient *registrationServerClient,
	config *NamedTunnelConfig,
	options *tunnelpogs.ConnectionOptions,
	connIndex uint8,
	observer *Observer,
) error {
	conn, err := rpcClient.client.RegisterConnection(
		ctx,
		config.Auth,
		config.ID,
		connIndex,
		options,
	)
	if err != nil {
		if err.Error() == DuplicateConnectionError {
			observer.metrics.regFail.WithLabelValues("dup_edge_conn", "registerConnection").Inc()
			return errDuplicationConnection
		}
		observer.metrics.regFail.WithLabelValues("server_error", "registerConnection").Inc()
		return serverRegistrationErrorFromRPC(err)
	}

	observer.metrics.regSuccess.WithLabelValues("registerConnection").Inc()

	observer.logServerInfo(connIndex, conn.Location, fmt.Sprintf("Connection %d registered with %s using ID %s", connIndex, conn.Location, conn.UUID))
	observer.sendConnectedEvent(connIndex, conn.Location)

	return nil
}

func (h *h2muxConnection) registerTunnel(ctx context.Context, credentialSetter CredentialManager, classicTunnel *ClassicTunnelConfig, registrationOptions *tunnelpogs.RegistrationOptions) error {
	h.observer.sendRegisteringEvent()

	stream, err := h.newRPCStream(ctx, register)
	if err != nil {
		return err
	}
	rpcClient := NewTunnelServerClient(ctx, stream, h.observer)
	defer rpcClient.Close()

	h.logServerInfo(ctx, rpcClient)
	registration := rpcClient.client.RegisterTunnel(
		ctx,
		classicTunnel.OriginCert,
		classicTunnel.Hostname,
		registrationOptions,
	)
	if registrationErr := registration.DeserializeError(); registrationErr != nil {
		// RegisterTunnel RPC failure
		return h.processRegisterTunnelError(registrationErr, register)
	}

	// Send free tunnel URL to UI
	h.observer.sendURL(registration.Url)
	credentialSetter.SetEventDigest(h.connIndex, registration.EventDigest)
	return h.processRegistrationSuccess(registration, register, credentialSetter, classicTunnel)
}

type CredentialManager interface {
	ReconnectToken() ([]byte, error)
	EventDigest(connID uint8) ([]byte, error)
	SetEventDigest(connID uint8, digest []byte)
	ConnDigest(connID uint8) ([]byte, error)
	SetConnDigest(connID uint8, digest []byte)
}

func (h *h2muxConnection) processRegistrationSuccess(
	registration *tunnelpogs.TunnelRegistration,
	name rpcName,
	credentialManager CredentialManager, classicTunnel *ClassicTunnelConfig,
) error {
	for _, logLine := range registration.LogLines {
		h.observer.Info(logLine)
	}

	if registration.TunnelID != "" {
		h.observer.metrics.tunnelsHA.AddTunnelID(h.connIndex, registration.TunnelID)
		h.observer.Infof("Each HA connection's tunnel IDs: %v", h.observer.metrics.tunnelsHA.String())
	}

	// Print out the user's trial zone URL in a nice box (if they requested and got one and UI flag is not set)
	if classicTunnel.IsTrialZone() {
		err := h.observer.logTrialHostname(registration)
		if err != nil {
			return err
		}
	}

	credentialManager.SetConnDigest(h.connIndex, registration.ConnDigest)
	h.observer.metrics.userHostnamesCounts.WithLabelValues(registration.Url).Inc()

	h.observer.Infof("Route propagating, it may take up to 1 minute for your new route to become functional")
	h.observer.metrics.regSuccess.WithLabelValues(string(name)).Inc()
	return nil
}

func (h *h2muxConnection) processRegisterTunnelError(err tunnelpogs.TunnelRegistrationError, name rpcName) error {
	if err.Error() == DuplicateConnectionError {
		h.observer.metrics.regFail.WithLabelValues("dup_edge_conn", string(name)).Inc()
		return errDuplicationConnection
	}
	h.observer.metrics.regFail.WithLabelValues("server_error", string(name)).Inc()
	return serverRegisterTunnelError{
		cause:     err,
		permanent: err.IsPermanent(),
	}
}

func (h *h2muxConnection) reconnectTunnel(ctx context.Context, credentialManager CredentialManager, classicTunnel *ClassicTunnelConfig, registrationOptions *tunnelpogs.RegistrationOptions) error {
	token, err := credentialManager.ReconnectToken()
	if err != nil {
		return err
	}
	eventDigest, err := credentialManager.EventDigest(h.connIndex)
	if err != nil {
		return err
	}
	connDigest, err := credentialManager.ConnDigest(h.connIndex)
	if err != nil {
		return err
	}

	h.observer.Debug("initiating RPC stream to reconnect")
	stream, err := h.newRPCStream(ctx, register)
	if err != nil {
		return err
	}
	rpcClient := NewTunnelServerClient(ctx, stream, h.observer)
	defer rpcClient.Close()

	h.logServerInfo(ctx, rpcClient)
	registration := rpcClient.client.ReconnectTunnel(
		ctx,
		token,
		eventDigest,
		connDigest,
		classicTunnel.Hostname,
		registrationOptions,
	)
	if registrationErr := registration.DeserializeError(); registrationErr != nil {
		// ReconnectTunnel RPC failure
		return h.processRegisterTunnelError(registrationErr, reconnect)
	}
	return h.processRegistrationSuccess(registration, reconnect, credentialManager, classicTunnel)
}

func (h *h2muxConnection) logServerInfo(ctx context.Context, rpcClient *tunnelServerClient) error {
	// Request server info without blocking tunnel registration; must use capnp library directly.
	serverInfoPromise := tunnelrpc.TunnelServer{Client: rpcClient.client.Client}.GetServerInfo(ctx, func(tunnelrpc.TunnelServer_getServerInfo_Params) error {
		return nil
	})
	serverInfoMessage, err := serverInfoPromise.Result().Struct()
	if err != nil {
		h.observer.Errorf("Failed to retrieve server information: %s", err)
		return err
	}
	serverInfo, err := tunnelpogs.UnmarshalServerInfo(serverInfoMessage)
	if err != nil {
		h.observer.Errorf("Failed to retrieve server information: %s", err)
		return err
	}
	h.observer.logServerInfo(h.connIndex, serverInfo.LocationName, fmt.Sprintf("Connnection %d connected to %s", h.connIndex, serverInfo.LocationName))
	return nil
}

func (h *h2muxConnection) unregister(isNamedTunnel bool) {
	unregisterCtx, cancel := context.WithTimeout(context.Background(), h.config.GracePeriod)
	defer cancel()

	stream, err := h.newRPCStream(unregisterCtx, register)
	if err != nil {
		return
	}

	if isNamedTunnel {
		rpcClient := newRegistrationRPCClient(unregisterCtx, stream, h.observer)
		defer rpcClient.close()

		rpcClient.client.UnregisterConnection(unregisterCtx)
	} else {
		rpcClient := NewTunnelServerClient(unregisterCtx, stream, h.observer)
		defer rpcClient.Close()

		// gracePeriod is encoded in int64 using capnproto
		rpcClient.client.UnregisterTunnel(unregisterCtx, h.config.GracePeriod.Nanoseconds())
	}
}
