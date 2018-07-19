package websocket

import (
  "crypto/tls"
  "io"
  "math/rand"
  "net/http"
  "testing"

  "github.com/sirupsen/logrus"
  "github.com/stretchr/testify/assert"

  "golang.org/x/net/websocket"

  "github.com/cloudflare/cloudflared/hello"
  "github.com/cloudflare/cloudflared/tlsconfig"
)

const (
  // example in Sec-Websocket-Key in rfc6455
  testSecWebsocketKey    = "dGhlIHNhbXBsZSBub25jZQ=="
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
  certPool, err := tlsconfig.LoadOriginCertPool(nil)
  assert.NoError(t, err)
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
  logger := logrus.New()
  shutdownC := make(chan struct{})
  errC := make(chan error)
  listener, err := hello.CreateTLSListener("localhost:1111")
  assert.NoError(t, err)
  defer listener.Close()

  go func() {
    errC <- hello.StartHelloWorldServer(logger, listener, shutdownC)
  }()

  req := testRequest(t, "https://localhost:1111/ws", nil)

  tlsConfig := websocketClientTLSConfig(t)
  assert.NotNil(t, tlsConfig)
  conn, resp, err := ClientConnect(req, tlsConfig)
  assert.NoError(t, err)
  assert.Equal(t, testSecWebsocketAccept, resp.Header.Get("Sec-WebSocket-Accept"))

  for i := 0; i < 1000; i++ {
    messageSize := rand.Int() % 2048 + 1
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

  conn.Close()
  close(shutdownC)
  <-errC
}
