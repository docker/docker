// Package pool contains helpers for pooling structures distinguishable by
// size.
//
// Quick example:
//
//   import "github.com/gobwas/pool"
//
//   func main() {
//      // Reuse objects in logarithmic range from 0 to 64 (0,1,2,4,6,8,16,32,64).
//      p := pool.New(0, 64)
//
//      buf, n := p.Get(10) // Returns buffer with 16 capacity.
//      if buf == nil {
//          buf = bytes.NewBuffer(make([]byte, n))
//      }
//      defer p.Put(buf, n)
//
//      // Work with buf.
//   }
//
// There are non-generic implementations for pooling:
// - pool/pbytes for []byte reuse;
// - pool/pbufio for *bufio.Reader and *bufio.Writer reuse;
//
package pool
