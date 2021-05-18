/*
Package ws implements a client and server for the WebSocket protocol as
specified in RFC 6455.

The main purpose of this package is to provide simple low-level API for
efficient work with protocol.

Overview.

Upgrade to WebSocket (or WebSocket handshake) can be done in two ways.

The first way is to use `net/http` server:

  http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	  conn, _, _, err := ws.UpgradeHTTP(r, w)
  })

The second and much more efficient way is so-called "zero-copy upgrade". It
avoids redundant allocations and copying of not used headers or other request
data. User decides by himself which data should be copied.

  ln, err := net.Listen("tcp", ":8080")
  if err != nil {
	  // handle error
  }

  conn, err := ln.Accept()
  if err != nil {
	  // handle error
  }

  handshake, err := ws.Upgrade(conn)
  if err != nil {
	  // handle error
  }

For customization details see `ws.Upgrader` documentation.

After WebSocket handshake you can work with connection in multiple ways.
That is, `ws` does not force the only one way of how to work with WebSocket:

  header, err := ws.ReadHeader(conn)
  if err != nil {
	  // handle err
  }

  buf := make([]byte, header.Length)
  _, err := io.ReadFull(conn, buf)
  if err != nil {
	  // handle err
  }

  resp := ws.NewBinaryFrame([]byte("hello, world!"))
  if err := ws.WriteFrame(conn, frame); err != nil {
      // handle err
  }

As you can see, it stream friendly:

  const N = 42

  ws.WriteHeader(ws.Header{
	  Fin:    true,
	  Length: N,
	  OpCode: ws.OpBinary,
  })

  io.CopyN(conn, rand.Reader, N)

Or:

  header, err := ws.ReadHeader(conn)
  if err != nil {
	  // handle err
  }

  io.CopyN(ioutil.Discard, conn, header.Length)

For more info see the documentation.
*/
package ws
