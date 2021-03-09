package websocket

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cloudflare/cloudflared/hello"
	"github.com/cloudflare/cloudflared/tlsconfig"
	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

const (
	// example in Sec-Websocket-Key in rfc6455
	testSecWebsocketKey = "dGhlIHNhbXBsZSBub25jZQ=="
	// example Sec-Websocket-Accept in rfc6455
	testSecWebsocketAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
)

func testRequest(t *testing.T, url string, stream io.ReadWriter) *http.Request {
	req, err := http.NewRequest("GET", url, stream)
	if err != nil {
		t.Fatalf("testRequestHeader error")
	}

	req.Header.Add("Connection", "Upgrade")
	req.Header.Add("Upgrade", "WebSocket")
	req.Header.Add("Sec-Websocket-Key", testSecWebsocketKey)
	req.Header.Add("Sec-Websocket-Protocol", "tunnel-protocol")
	req.Header.Add("Sec-Websocket-Version", "13")
	req.Header.Add("User-Agent", "curl/7.59.0")

	return req
}

func websocketClientTLSConfig(t *testing.T) *tls.Config {
	certPool := x509.NewCertPool()
	helloCert, err := tlsconfig.GetHelloCertificateX509()
	assert.NoError(t, err)
	certPool.AddCert(helloCert)
	assert.NotNil(t, certPool)
	return &tls.Config{RootCAs: certPool}
}

func TestWebsocketHeaders(t *testing.T) {
	req := testRequest(t, "http://example.com", nil)
	wsHeaders := websocketHeaders(req)
	for _, header := range stripWebsocketHeaders {
		assert.Empty(t, wsHeaders[header])
	}
	assert.Equal(t, "curl/7.59.0", wsHeaders.Get("User-Agent"))
}

func TestGenerateAcceptKey(t *testing.T) {
	req := testRequest(t, "http://example.com", nil)
	assert.Equal(t, testSecWebsocketAccept, generateAcceptKey(req))
}

func TestServe(t *testing.T) {
	log := zerolog.Nop()
	shutdownC := make(chan struct{})
	errC := make(chan error)
	listener, err := hello.CreateTLSListener("localhost:1111")
	assert.NoError(t, err)
	defer listener.Close()

	go func() {
		errC <- hello.StartHelloWorldServer(&log, listener, shutdownC)
	}()

	req := testRequest(t, "https://localhost:1111/ws", nil)

	tlsConfig := websocketClientTLSConfig(t)
	assert.NotNil(t, tlsConfig)
	d := gws.Dialer{TLSClientConfig: tlsConfig}
	conn, resp, err := ClientConnect(req, &d)
	assert.NoError(t, err)
	assert.Equal(t, testSecWebsocketAccept, resp.Header.Get("Sec-WebSocket-Accept"))

	for i := 0; i < 1000; i++ {
		messageSize := rand.Int()%2048 + 1
		clientMessage := make([]byte, messageSize)
		// rand.Read always returns len(clientMessage) and a nil error
		rand.Read(clientMessage)
		err = conn.WriteMessage(websocket.BinaryFrame, clientMessage)
		assert.NoError(t, err)

		messageType, message, err := conn.ReadMessage()
		assert.NoError(t, err)
		assert.Equal(t, websocket.BinaryFrame, messageType)
		assert.Equal(t, clientMessage, message)
	}

	_ = conn.Close()
	close(shutdownC)
	<-errC
}

func TestWebsocketWrapper(t *testing.T) {

	listener, err := hello.CreateTLSListener("localhost:0")
	require.NoError(t, err)

	serverErrorChan := make(chan error)
	helloSvrCtx, cancelHelloSvr := context.WithCancel(context.Background())
	defer func() { <-serverErrorChan }()
	defer cancelHelloSvr()
	go func() {
		log := zerolog.Nop()
		serverErrorChan <- hello.StartHelloWorldServer(&log, listener, helloSvrCtx.Done())
	}()

	tlsConfig := websocketClientTLSConfig(t)
	d := gws.Dialer{TLSClientConfig: tlsConfig, HandshakeTimeout: time.Minute}
	testAddr := fmt.Sprintf("https://%s/ws", listener.Addr().String())
	req := testRequest(t, testAddr, nil)
	conn, resp, err := ClientConnect(req, &d)
	require.NoError(t, err)
	require.Equal(t, testSecWebsocketAccept, resp.Header.Get("Sec-WebSocket-Accept"))

	// Websocket now connected to test server so lets check our wrapper
	wrapper := GorillaConn{Conn: conn}
	buf := make([]byte, 100)
	wrapper.Write([]byte("abc"))
	n, err := wrapper.Read(buf)
	require.NoError(t, err)
	require.Equal(t, n, 3)
	require.Equal(t, "abc", string(buf[:n]))

	// Test partial read, read 1 of 3 bytes in one read and the other 2 in another read
	wrapper.Write([]byte("abc"))
	buf = buf[:1]
	n, err = wrapper.Read(buf)
	require.NoError(t, err)
	require.Equal(t, n, 1)
	require.Equal(t, "a", string(buf[:n]))
	buf = buf[:cap(buf)]
	n, err = wrapper.Read(buf)
	require.NoError(t, err)
	require.Equal(t, n, 2)
	require.Equal(t, "bc", string(buf[:n]))
}

// func TestStartProxyServer(t *testing.T) {
// 	var wg sync.WaitGroup
// 	remoteAddress := "localhost:1113"
// 	listenerAddress := "localhost:1112"
// 	message := "Good morning Austin! Time for another sunny day in the great state of Texas."
// 	logger := zerolog.Nop()
// 	shutdownC := make(chan struct{})

// 	listener, err := net.Listen("tcp", listenerAddress)
// 	assert.NoError(t, err)
// 	defer listener.Close()

// 	remoteListener, err := net.Listen("tcp", remoteAddress)
// 	assert.NoError(t, err)
// 	defer remoteListener.Close()

// 	wg.Add(1)
// 	go func() {
// 		defer wg.Done()
// 		conn, err := remoteListener.Accept()
// 		assert.NoError(t, err)
// 		buf := make([]byte, len(message))
// 		conn.Read(buf)
// 		assert.Equal(t, string(buf), message)
// 	}()

// 	go func() {
// 		StartProxyServer(logger, listener, remoteAddress, shutdownC)
// 	}()

// 	req := testRequest(t, fmt.Sprintf("http://%s/", listenerAddress), nil)
// 	conn, _, err := ClientConnect(req, nil)
// 	assert.NoError(t, err)
// 	err = conn.WriteMessage(1, []byte(message))
// 	assert.NoError(t, err)
// 	wg.Wait()
// }
