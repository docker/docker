// Package nlwrap wraps vishvandanda/netlink functions that may return EINTR.
//
// A Handle instantiated using [NewHandle] or [NewHandleAt] can be used in place
// of a netlink.Handle, it's a wrapper that replaces methods that need to be
// wrapped. Functions that use the package handle need to be called as "nlwrap.X"
// instead of "netlink.X".
//
// When netlink.ErrDumpInterrupted is returned, the wrapped functions retry up to
// maxAttempts times. This error means NLM_F_DUMP_INTR was flagged in a netlink
// response, meaning something changed during the dump so results may be
// incomplete or inconsistent.
//
// To avoid retrying indefinitely, if netlink.ErrDumpInterrupted is still
// returned after maxAttempts, the wrapped functions will discard the error, log
// a stack trace to make the issue visible and aid in debugging, and return the
// possibly inconsistent results. Returning possibly inconsistent results matches
// the behaviour of vishvananda/netlink versions prior to 1.2.1, in which the
// NLM_F_DUMP_INTR flag was ignored.
package nlwrap

import (
	"context"
	"fmt"
	"runtime"

	"github.com/containerd/log"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Arbitrary limit on max attempts at netlink calls if they are repeatedly interrupted.
const maxAttempts = 5

type Handle struct {
	*netlink.Handle
}

func NewHandle(nlFamilies ...int) (Handle, error) {
	nlh, err := netlink.NewHandle(nlFamilies...)
	if err != nil {
		return Handle{}, err
	}
	return Handle{nlh}, nil
}

// NewHandleAt creates a new netlink handle in the specified network namespace.
//
// Unlike netlink.NewHandleAt, this function properly manages thread lifecycle
// when the calling thread's network namespace cannot be restored after switching
// (e.g. in rootless mode where setns back to the host netns fails with EPERM).
// The upstream netlink library silently ignores setns restoration errors and
// returns the tainted thread to the Go runtime's thread pool, which causes
// goroutines scheduled on those threads to operate in the wrong network
// namespace.
func NewHandleAt(ns netns.NsHandle, nlFamilies ...int) (Handle, error) {
	if !ns.IsOpen() {
		// No target namespace; same as NewHandle.
		return NewHandle(nlFamilies...)
	}

	type result struct {
		handle *netlink.Handle
		err    error
	}
	ch := make(chan result, 1)

	go func() {
		runtime.LockOSThread()

		origNS, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			ch <- result{err: fmt.Errorf("could not get current network namespace: %w", err)}
			return
		}
		defer origNS.Close()

		if err := netns.Set(ns); err != nil {
			runtime.UnlockOSThread()
			ch <- result{err: fmt.Errorf("failed to enter network namespace: %w", err)}
			return
		}

		// Create netlink sockets in the target namespace.
		// NewHandle with no ns args does not do any namespace switching.
		nlh, err := netlink.NewHandle(nlFamilies...)
		if err != nil {
			// Best-effort restore before reporting the error.
			netns.Set(origNS) //nolint:errcheck
			runtime.UnlockOSThread()
			ch <- result{err: err}
			return
		}

		if err := netns.Set(origNS); err != nil {
			// Cannot restore the thread's network namespace. Keep the
			// goroutine locked to this thread so the Go runtime terminates
			// it instead of returning a tainted thread to the pool.
			ch <- result{handle: nlh}
			return
		}
		runtime.UnlockOSThread()
		ch <- result{handle: nlh}
	}()

	r := <-ch
	if r.err != nil {
		return Handle{}, r.err
	}
	return Handle{r.handle}, nil
}

func (nlh Handle) Close() {
	if nlh.Handle != nil {
		nlh.Handle.Close()
	}
}

func retryOnIntr(f func() error) {
	for range maxAttempts {
		if err := f(); !errors.Is(err, netlink.ErrDumpInterrupted) {
			return
		}
	}
	log.G(context.TODO()).Infof("netlink call interrupted after %d attempts", maxAttempts)
}

func discardErrDumpInterrupted(err error) error {
	if errors.Is(err, netlink.ErrDumpInterrupted) {
		// The netlink function has returned possibly-inconsistent data along with the
		// error. Discard the error and return the data. This restores the behaviour of
		// the netlink package prior to v1.2.1, in which NLM_F_DUMP_INTR was ignored in
		// the netlink response.
		log.G(context.TODO()).Warnf("discarding ErrDumpInterrupted: %+v", errors.WithStack(err))
		return nil
	}
	return err
}

// AddrList calls nlh.Handle.AddrList, retrying if necessary.
func (nlh Handle) AddrList(link netlink.Link, family int) (addrs []netlink.Addr, err error) {
	retryOnIntr(func() error {
		addrs, err = nlh.Handle.AddrList(link, family) //nolint:forbidigo
		return err
	})
	return addrs, discardErrDumpInterrupted(err)
}

// AddrList calls netlink.AddrList, retrying if necessary.
func AddrList(link netlink.Link, family int) (addrs []netlink.Addr, err error) {
	retryOnIntr(func() error {
		addrs, err = netlink.AddrList(link, family) //nolint:forbidigo
		return err
	})
	return addrs, discardErrDumpInterrupted(err)
}

// ConntrackDeleteFilters calls nlh.Handle.ConntrackDeleteFilters, retrying if necessary.
func (nlh Handle) ConntrackDeleteFilters(
	table netlink.ConntrackTableType,
	family netlink.InetFamily,
	filters ...netlink.CustomConntrackFilter,
) (matched uint, err error) {
	retryOnIntr(func() error {
		matched, err = nlh.Handle.ConntrackDeleteFilters(table, family, filters...) //nolint:forbidigo
		return err
	})
	return matched, discardErrDumpInterrupted(err)
}

// ConntrackTableList calls netlink.ConntrackTableList, retrying if necessary.
func ConntrackTableList(
	table netlink.ConntrackTableType,
	family netlink.InetFamily,
) (flows []*netlink.ConntrackFlow, err error) {
	retryOnIntr(func() error {
		flows, err = netlink.ConntrackTableList(table, family) //nolint:forbidigo
		return err
	})
	return flows, discardErrDumpInterrupted(err)
}

// LinkByName calls nlh.Handle.LinkByName, retrying if necessary. The netlink function
// doesn't normally ask the kernel for a dump of links. But, on an old kernel, it
// will do as a fallback and that dump may get inconsistent results.
func (nlh Handle) LinkByName(name string) (link netlink.Link, err error) {
	retryOnIntr(func() error {
		link, err = nlh.Handle.LinkByName(name) //nolint:forbidigo
		return err
	})
	return link, discardErrDumpInterrupted(err)
}

// LinkByName calls netlink.LinkByName, retrying if necessary. The netlink
// function doesn't normally ask the kernel for a dump of links. But, on an old
// kernel, it will do as a fallback and that dump may get inconsistent results.
func LinkByName(name string) (link netlink.Link, err error) {
	retryOnIntr(func() error {
		link, err = netlink.LinkByName(name) //nolint:forbidigo
		return err
	})
	return link, discardErrDumpInterrupted(err)
}

// LinkList calls nlh.Handle.LinkList, retrying if necessary.
func (nlh Handle) LinkList() (links []netlink.Link, err error) {
	retryOnIntr(func() error {
		links, err = nlh.Handle.LinkList() //nolint:forbidigo
		return err
	})
	return links, discardErrDumpInterrupted(err)
}

// LinkList calls netlink.Handle.LinkList, retrying if necessary.
func LinkList() (links []netlink.Link, err error) {
	retryOnIntr(func() error {
		links, err = netlink.LinkList() //nolint:forbidigo
		return err
	})
	return links, discardErrDumpInterrupted(err)
}

// LinkSubscribeWithOptions calls netlink.LinkSubscribeWithOptions, retrying if
// necessary. Close the done channel when done (rather than just sending on it),
// so that goroutines started by the netlink package are all stopped.
//
// When a target namespace is specified, the subscribe socket is created on a
// dedicated OS thread to avoid the same executeInNetns thread contamination
// issue described in [NewHandleAt].
func LinkSubscribeWithOptions(ch chan<- netlink.LinkUpdate, done <-chan struct{}, options netlink.LinkSubscribeOptions) (err error) {
	if options.Namespace != nil && options.Namespace.IsOpen() {
		ns := *options.Namespace
		// Clear the namespace option so the netlink library does not do
		// its own namespace switching (via executeInNetns). We handle it.
		options.Namespace = nil
		errCh := make(chan error, 1)
		go func() {
			runtime.LockOSThread()

			origNS, nserr := netns.Get()
			if nserr != nil {
				runtime.UnlockOSThread()
				errCh <- fmt.Errorf("could not get current network namespace: %w", nserr)
				return
			}
			defer origNS.Close()

			if nserr := netns.Set(ns); nserr != nil {
				runtime.UnlockOSThread()
				errCh <- fmt.Errorf("failed to enter network namespace: %w", nserr)
				return
			}

			// Create the subscribe socket in the target namespace.
			// With Namespace cleared, the netlink library will not
			// attempt any namespace switching internally.
			retryOnIntr(func() error {
				err = netlink.LinkSubscribeWithOptions(ch, done, options) //nolint:forbidigo
				return err
			})
			errCh <- err

			if nserr := netns.Set(origNS); nserr != nil {
				// Cannot restore: keep locked so the runtime kills
				// this thread instead of returning it to the pool.
				return
			}
			runtime.UnlockOSThread()
		}()
		return <-errCh
	}

	retryOnIntr(func() error {
		err = netlink.LinkSubscribeWithOptions(ch, done, options) //nolint:forbidigo
		return err
	})
	return err
}

// NeighList calls nlh.Handle.NeighList, retrying if necessary.
func (nlh Handle) NeighList(linkIndex, family int) (neighbors []netlink.Neigh, err error) {
	retryOnIntr(func() error {
		neighbors, err = nlh.Handle.NeighList(linkIndex, family) //nolint:forbidigo
		return err
	})
	return neighbors, discardErrDumpInterrupted(err)
}

// RouteList calls nlh.Handle.RouteList, retrying if necessary.
func (nlh Handle) RouteList(link netlink.Link, family int) (routes []netlink.Route, err error) {
	retryOnIntr(func() error {
		routes, err = nlh.Handle.RouteList(link, family) //nolint:forbidigo
		return err
	})
	return routes, discardErrDumpInterrupted(err)
}

// RouteListFiltered calls nlh.Handle.RouteListFiltered, retrying if necessary.
func (nlh Handle) RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) (routes []netlink.Route, err error) {
	retryOnIntr(func() error {
		routes, err = nlh.Handle.RouteListFiltered(family, filter, filterMask) //nolint:forbidigo
		return err
	})
	return routes, discardErrDumpInterrupted(err)
}

// RuleList calls nlh.Handle.RuleList, retrying if necessary.
func (nlh Handle) RuleList(family int) (rules []netlink.Rule, err error) {
	retryOnIntr(func() error {
		rules, err = nlh.Handle.RuleList(family) //nolint:forbidigo
		return err
	})
	return rules, discardErrDumpInterrupted(err)
}

// XfrmPolicyList calls nlh.Handle.XfrmPolicyList, retrying if necessary.
func (nlh Handle) XfrmPolicyList(family int) (policies []netlink.XfrmPolicy, err error) {
	retryOnIntr(func() error {
		policies, err = nlh.Handle.XfrmPolicyList(family) //nolint:forbidigo
		return err
	})
	return policies, discardErrDumpInterrupted(err)
}

// XfrmStateList calls nlh.Handle.XfrmStateList, retrying if necessary.
func (nlh Handle) XfrmStateList(family int) (states []netlink.XfrmState, err error) {
	retryOnIntr(func() error {
		states, err = nlh.Handle.XfrmStateList(family) //nolint:forbidigo
		return err
	})
	return states, discardErrDumpInterrupted(err)
}
