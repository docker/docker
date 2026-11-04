package libnetwork

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/containerd/log"
	"github.com/miekg/dns"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/sirupsen/logrus"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// a simple/null address type that will be used to fake a local address for unit testing
type tstaddr struct {
	network string
}

func (a *tstaddr) Network() string {
	if a.network != "" {
		return a.network
	}
	return "tcp"
}

func (a *tstaddr) String() string { return "(fake)" }

// a simple writer that implements dns.ResponseWriter for unit testing purposes
type tstwriter struct {
	network string
	msg     *dns.Msg
}

func (w *tstwriter) WriteMsg(m *dns.Msg) (err error) {
	// Assert that the message is serializable.
	if _, err := m.Pack(); err != nil {
		return err
	}
	w.msg = m
	return nil
}

func (w *tstwriter) Write(m []byte) (int, error) { return 0, nil }

func (w *tstwriter) LocalAddr() net.Addr {
	return &tstaddr{network: w.network}
}

func (w *tstwriter) RemoteAddr() net.Addr {
	return &tstaddr{network: w.network}
}

func (w *tstwriter) TsigStatus() error { return nil }

func (w *tstwriter) TsigTimersOnly(b bool) {}

func (w *tstwriter) Hijack() {}

func (w *tstwriter) Close() error { return nil }

func (w *tstwriter) GetResponse() *dns.Msg { return w.msg }

func (w *tstwriter) ClearResponse() { w.msg = nil }

func checkNonNullResponse(t *testing.T, m *dns.Msg) {
	t.Helper()
	if m == nil {
		t.Fatal("Null DNS response found. Non Null response msg expected.")
	}
}

func checkDNSAnswersCount(t *testing.T, m *dns.Msg, expected int) {
	t.Helper()
	answers := len(m.Answer)
	if answers != expected {
		t.Fatalf("Expected number of answers in response: %d. Found: %d", expected, answers)
	}
}

func checkDNSResponseCode(t *testing.T, m *dns.Msg, expected int) {
	t.Helper()
	if m.MsgHdr.Rcode != expected {
		t.Fatalf("Expected DNS response code: %d (%s). Found: %d (%s)", expected, dns.RcodeToString[expected], m.MsgHdr.Rcode, dns.RcodeToString[m.MsgHdr.Rcode])
	}
}

func checkDNSRRType(t *testing.T, actual, expected uint16) {
	t.Helper()
	if actual != expected {
		t.Fatalf("Expected DNS Rrtype: %d. Found: %d", expected, actual)
	}
}

func newDNSHandlerServFailOnce(requests *atomic.Int32) func(w dns.ResponseWriter, r *dns.Msg) {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Compress = false
		if requests.Add(1) == 1 {
			m.SetRcode(r, dns.RcodeServerFailure)
		}
		if err := w.WriteMsg(m); err != nil {
			log.G(context.TODO()).WithError(err).Error("Error writing dns response")
		}
	}
}

// Packet 24 extracted from
// https://gist.github.com/vojtad/3bac63b8c91b1ec50e8d8b36047317fa/raw/7d75eb3d3448381bf252ae55ea5123a132c46658/host.pcap
// (https://github.com/moby/moby/issues/44575)
// which is a non-compliant DNS reply > 512B (w/o EDNS(0)) to the query
//
//	s3.amazonaws.com. IN A
const oversizedDNSReplyMsg = "\xf5\x11\x81\x80\x00\x01\x00\x20\x00\x00\x00\x00\x02\x73\x33\x09" +
	"\x61\x6d\x61\x7a\x6f\x6e\x61\x77\x73\x03\x63\x6f\x6d\x00\x00\x01" +
	"\x00\x01\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\x11\x9e\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\x4c\x66\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\xda\x10\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\x01\x3e\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\x88\x68\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\x66\x9e\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\x5f\x28\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\x8e\x4e\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x36\xe7" +
	"\x84\xf0\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x34\xd8" +
	"\x92\x45\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\x8f\xa6\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x36\xe7" +
	"\xc0\xd0\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\xfe\x28\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\xaa\x3d\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\x4e\x56\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd9" +
	"\xea\xb0\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\x6d\xed\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x04\x00\x04\x34\xd8" +
	"\x28\x00\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x34\xd9" +
	"\xe9\x78\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x34\xd9" +
	"\x6e\x9e\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x34\xd9" +
	"\x45\x86\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x34\xd8" +
	"\x30\x38\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x36\xe7" +
	"\xc6\xa8\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x00\x00\x04\x03\x05" +
	"\x01\x9d\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd9" +
	"\xa8\xe8\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd9" +
	"\x64\xa6\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd8" +
	"\x3c\x48\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd8" +
	"\x35\x20\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd9" +
	"\x54\xf6\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd9" +
	"\x5d\x36\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x34\xd9" +
	"\x30\x36\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x05\x00\x04\x36\xe7" +
	"\x83\x90"

// Regression test for https://github.com/moby/moby/issues/44575
func TestOversizedDNSReply(t *testing.T) {
	srv, err := net.ListenPacket("udp", "127.0.0.1:0")
	assert.NilError(t, err)
	defer srv.Close()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, src, err := srv.ReadFrom(buf)
			if errors.Is(err, net.ErrClosed) {
				return
			}
			t.Logf("[<-%v]\n%s", src, hex.Dump(buf[:n]))
			if n < 2 {
				continue
			}
			resp := []byte(oversizedDNSReplyMsg)
			resp[0], resp[1] = buf[0], buf[1] // Copy query ID into response.
			_, err = srv.WriteTo(resp, src)
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				t.Log(err)
			}
		}
	}()

	srvAddr := srv.LocalAddr().(*net.UDPAddr)
	rsv := NewResolver("", true, noopDNSBackend{})
	// The resolver logs lots of valuable info at level debug. Redirect it
	// to t.Log() so the log spew is emitted only if the test fails.
	rsv.logger = testLogger(t)
	rsv.SetExtServers([]extDNSEntry{
		{IPStr: srvAddr.IP.String(), port: uint16(srvAddr.Port), HostLoopback: true},
	})

	w := &tstwriter{network: srvAddr.Network()}
	q := new(dns.Msg).SetQuestion("s3.amazonaws.com.", dns.TypeA)
	rsv.serveDNS(w, q)
	resp := w.GetResponse()
	checkNonNullResponse(t, resp)
	t.Log("Response: ", resp.String())
	checkDNSResponseCode(t, resp, dns.RcodeSuccess)
	assert.Assert(t, len(resp.Answer) >= 1)
	checkDNSRRType(t, resp.Answer[0].Header().Rrtype, dns.TypeA)
}

func testLogger(t *testing.T) *logrus.Entry {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(tlogWriter{t})
	return logrus.NewEntry(logger)
}

type tlogWriter struct{ t *testing.T }

func (w tlogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

type noopDNSBackend struct{ DNSBackend }

func (noopDNSBackend) ResolveName(_ context.Context, name string, ipType types.IPFamily) ([]net.IP, bool) {
	return nil, false
}

func (noopDNSBackend) ExecFunc(f func()) error { f(); return nil }

func (noopDNSBackend) NdotsSet() bool { return false }
func (noopDNSBackend) Ndots() int     { return 0 }

func (noopDNSBackend) HandleQueryResp(name string, ip net.IP) {}

func TestReplySERVFAIL(t *testing.T) {
	cases := []struct {
		name     string
		q        *dns.Msg
		proxyDNS bool
	}{
		{
			name: "InternalError",
			q:    new(dns.Msg).SetQuestion("_sip._tcp.example.com.", dns.TypeSRV),
		},
		{
			name: "ProxyDNS=false",
			q:    new(dns.Msg).SetQuestion("example.com.", dns.TypeA),
		},
		{
			name:     "ProxyDNS=true", // No extDNS servers configured -> no answer from any upstream
			q:        new(dns.Msg).SetQuestion("example.com.", dns.TypeA),
			proxyDNS: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rsv := NewResolver("", tt.proxyDNS, badSRVDNSBackend{})
			rsv.logger = testLogger(t)
			w := &tstwriter{}
			rsv.serveDNS(w, tt.q)
			resp := w.GetResponse()
			checkNonNullResponse(t, resp)
			t.Log("Response: ", resp.String())
			checkDNSResponseCode(t, resp, dns.RcodeServerFailure)
		})
	}
}

type badSRVDNSBackend struct{ noopDNSBackend }

func (badSRVDNSBackend) ResolveService(_ context.Context, _ string) ([]*net.SRV, []net.IP) {
	return []*net.SRV{nil, nil, nil}, nil // Mismatched slice lengths
}

func TestProxyNXDOMAIN(t *testing.T) {
	mockSOA, err := dns.NewRR(".	86367	IN	SOA	a.root-servers.net. nstld.verisign-grs.com. 2023051800 1800 900 604800 86400\n")
	assert.NilError(t, err)
	assert.Assert(t, mockSOA != nil)

	serveStarted := make(chan struct{})
	srv := &dns.Server{
		Net:  "udp",
		Addr: "127.0.0.1:0",
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			msg := new(dns.Msg).SetRcode(r, dns.RcodeNameError)
			msg.Ns = append(msg.Ns, dns.Copy(mockSOA))
			w.WriteMsg(msg)
		}),
		NotifyStartedFunc: func() { close(serveStarted) },
	}
	serveDone := make(chan error, 1)
	go func() {
		defer close(serveDone)
		serveDone <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveDone:
		t.Fatal(err)
	case <-serveStarted:
	}

	defer func() {
		if err := srv.Shutdown(); err != nil {
			t.Error(err)
		}
		<-serveDone
	}()

	// This test, by virtue of running a server and client in different
	// not-locked-to-thread goroutines, happens to be a good canary for
	// whether we are leaking unlocked OS threads set to the wrong network
	// namespace. Make a best-effort attempt to detect that situation so we
	// are not left chasing ghosts next time.
	netnsutils.AssertSocketSameNetNS(t, srv.PacketConn.(*net.UDPConn))

	srvAddr := srv.PacketConn.LocalAddr().(*net.UDPAddr)
	rsv := NewResolver("", true, noopDNSBackend{})
	rsv.SetExtServers([]extDNSEntry{
		{IPStr: srvAddr.IP.String(), port: uint16(srvAddr.Port), HostLoopback: true},
	})

	// The resolver logs lots of valuable info at level debug. Redirect it
	// to t.Log() so the log spew is emitted only if the test fails.
	rsv.logger = testLogger(t)

	w := &tstwriter{network: srvAddr.Network()}
	q := new(dns.Msg).SetQuestion("example.net.", dns.TypeA)
	rsv.serveDNS(w, q)
	resp := w.GetResponse()
	checkNonNullResponse(t, resp)
	t.Log("Response:\n" + resp.String())
	checkDNSResponseCode(t, resp, dns.RcodeNameError)
	assert.Assert(t, is.Len(resp.Answer, 0))
	assert.Assert(t, is.Len(resp.Ns, 1))
	assert.Equal(t, resp.Ns[0].String(), mockSOA.String())
}

type ptrDNSBackend struct {
	noopDNSBackend
	zone map[string]string
}

func (b *ptrDNSBackend) ResolveIP(_ context.Context, name string) string {
	return b.zone[name]
}

// Regression test for https://github.com/moby/moby/issues/46928
func TestInvalidReverseDNS(t *testing.T) {
	rsv := NewResolver("", false, &ptrDNSBackend{zone: map[string]string{"4.3.2.1": "sixtyfourcharslong9012345678901234567890123456789012345678901234"}})
	rsv.logger = testLogger(t)

	w := &tstwriter{}
	q := new(dns.Msg).SetQuestion("4.3.2.1.in-addr.arpa.", dns.TypePTR)
	rsv.serveDNS(w, q)
	resp := w.GetResponse()
	checkNonNullResponse(t, resp)
	t.Log("Response: ", resp.String())
	checkDNSResponseCode(t, resp, dns.RcodeServerFailure)
}
