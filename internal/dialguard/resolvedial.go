package dialguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// defaultFallbackDelay is how long the second address family waits before it
// starts, and it is net.Dialer's own default rather than a number chosen here:
// this dialler replaces net.Dialer's resolution, so a deployment's dual-stack
// behaviour must not change with it.
const defaultFallbackDelay = 300 * time.Millisecond

// minPartialTimeout floors the per-address share of the budget, so a name with
// many addresses does not give each one a slice too short to complete a
// handshake on a slow network. net.Dialer applies the same floor for the same
// reason.
const minPartialTimeout = 2 * time.Second

// Dialer connects by resolving a name **once**, judging every address that came
// back, and dialling what survives.
//
// It exists because the platform's decisions are made about names while the
// socket goes to an address, and until this type the resolution that turned one
// into the other happened inside net.Dialer — below every decision, visible only
// to the Control hook one syscall before connect(2). A gate that has admitted a
// host, or an executor that has chosen a credential for it, cannot see where the
// connection is actually going, so nothing above the socket can be said about
// it (#601, docs/plan/44).
//
// What that buys is one resolution per dial, with the addresses the floor judged
// being exactly the addresses connect(2) is called on. What it does not buy is a
// different answer from the resolver: a `search` list still completes a relative
// name, and IPAllowed still admits the RFC 1918 address that completion may
// return, deliberately (see this package's doc comment). Closing that is a
// policy decision about *which name may answer*, and it belongs above this type.
//
// The zero value is usable and behaves like net.Dialer with the platform floor:
// no overall timeout, the process resolver, IPAllowed.
type Dialer struct {
	// Timeout bounds the whole dial — every address attempt together, as
	// net.Dialer.Timeout does. Zero means no bound.
	Timeout time.Duration

	// FallbackDelay is how long the second address family waits before racing
	// the first (RFC 6555 "Happy Eyeballs"). Zero selects
	// defaultFallbackDelay; negative disables the race, as it does on
	// net.Dialer.
	FallbackDelay time.Duration

	// Lookup resolves host to addresses. network is "ip", "ip4" or "ip6", the
	// spelling net.Resolver.LookupIP takes, so nil selects
	// net.DefaultResolver.LookupIP and a test can substitute the whole
	// resolution without a DNS server. The answer's order is honoured: a
	// resolver has already applied RFC 6724 to it.
	Lookup func(ctx context.Context, network, host string) ([]net.IP, error)

	// Allow judges every resolved address before any connect. Nil selects
	// IPAllowed.
	//
	// It takes a context because a caller's answer can depend on one: the gate
	// floors a dial for some admission classes and not others, and the class
	// travels in the request context. It is called once per address, never per
	// connection attempt, so an address refused here is never dialled at all.
	Allow func(ctx context.Context, ip net.IP) error

	// dialOne opens one socket to one literal address. It is unexported and
	// unset in production, where the standard dialler is the only thing that
	// should ever open a socket; a test sets it to stand in for the network,
	// which is the only way to drive a blackholed address — the case the
	// fallback delay exists for — without depending on one.
	dialOne func(ctx context.Context, network, addr string) (net.Conn, error)
}

// DialContext connects to addr, which is a "host:port" the caller has already
// decided may be reached. It is a drop-in for (&net.Dialer{…}).DialContext.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// The bound covers the lookup as well as the connects, which is where
	// net.Dialer applies its own Timeout and is the only placement that bounds
	// the whole operation: a resolver that never answers would otherwise hold
	// the caller for as long as its context allows, and for the gate that
	// context is the sandbox's request. Cancelling on return cannot disturb a
	// connection that came back — a dial stops watching its context once it has
	// one to hand over.
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not an address this can take apart, which is a caller's malformed
		// input rather than a destination. It goes to the standard dialler so
		// it fails exactly as it did before this type existed, with that
		// dialler's own error — the same rule gate.rootedName argues in place
		// for the same shapes.
		return d.dial(ctx, network, addr)
	}
	if ip, ok := parseAddr(host); ok {
		// A literal skips the lookup, and must: resolving one would be a
		// different operation with different failures. The zone of a
		// zone-scoped address is not part of what an address check is about,
		// so the floor sees the address without it while the dial keeps it.
		if err := d.allow(ctx, ipOf(ip)); err != nil {
			return nil, err
		}
		return d.dial(ctx, network, addr)
	}

	ips, err := d.lookup(ctx, network, host)
	if err != nil {
		return nil, err
	}
	addrs, err := d.admitted(ctx, ips)
	if err != nil {
		return nil, err
	}
	return d.dialAll(ctx, network, addrs, port)
}

// admitted runs the floor over every resolved address and returns the ones it
// admits. It is an error rather than an empty slice when none survive, and the
// error is the first refusal so that a caller matching on ErrRefused — which
// vaultresolve does, to tell a destination that can never be dialled from a
// network that may recover — still sees it.
func (d *Dialer) admitted(ctx context.Context, ips []net.IP) ([]netip.Addr, error) {
	var (
		out     []netip.Addr
		refusal error
	)
	for _, ip := range ips {
		if err := d.allow(ctx, ip); err != nil {
			if refusal == nil {
				refusal = err
			}
			continue
		}
		a, ok := parseAddr(ip.String())
		if !ok {
			// An address the floor admitted but netip cannot read is refused
			// rather than dialled: IPAllowed answers for a net.IP it can read,
			// and a caller's own Allow may be laxer than that.
			if refusal == nil {
				refusal = fmt.Errorf("dial target is not a usable address: %w", ErrRefused)
			}
			continue
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		if refusal != nil {
			return nil, refusal
		}
		// A resolver that answers with no addresses and no error. Nothing in
		// the standard library does this, but Lookup is a seam.
		return nil, fmt.Errorf("dial target resolved to no address: %w", ErrRefused)
	}
	return out, nil
}

// dialAll connects to the first of addrs that answers, reproducing the three
// things net.Dialer does with a resolved list and a bare literal dial does not:
// failover to the next address, a per-address share of the budget so one
// blackholed address cannot spend all of it, and the other family started after
// FallbackDelay so a broken IPv6 path costs that delay rather than a connect
// timeout.
func (d *Dialer) dialAll(ctx context.Context, network string, addrs []netip.Addr, port string) (net.Conn, error) {
	primary, fallback := partition(addrs)
	if len(fallback) == 0 || d.FallbackDelay < 0 {
		return d.dialSerial(ctx, network, append(primary, fallback...), port)
	}
	return d.dialRace(ctx, network, primary, fallback, port)
}

// dialSerial tries each address in turn, giving each a share of what is left of
// the deadline, and returns the first error when none answer.
func (d *Dialer) dialSerial(ctx context.Context, network string, addrs []netip.Addr, port string) (net.Conn, error) {
	var first error
	for i, a := range addrs {
		actx, cancel := partialDeadline(ctx, len(addrs)-i)
		c, err := d.dial(actx, network, joinAddrPort(a, port))
		cancel()
		if err == nil {
			return c, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if first == nil {
		// Only reachable with an empty list, which admitted() cannot produce.
		first = errors.New("no address to dial")
	}
	return nil, first
}

// dialRace runs the two families against each other, the second starting after
// FallbackDelay or as soon as the first has failed outright. The first
// connection to come up wins; the loser is cancelled and its connection, if it
// established one in the meantime, is closed rather than leaked.
//
// When both fail the primary family's error is the one returned, so the message
// names the family the resolver preferred.
func (d *Dialer) dialRace(ctx context.Context, network string, primary, fallback []netip.Addr, port string) (net.Conn, error) {
	type outcome struct {
		conn    net.Conn
		err     error
		primary bool
	}
	rctx, cancel := context.WithCancel(ctx)
	results := make(chan outcome, 2)
	pending := 0
	start := func(addrs []netip.Addr, isPrimary bool) {
		pending++
		go func() {
			c, err := d.dialSerial(rctx, network, addrs, port)
			results <- outcome{conn: c, err: err, primary: isPrimary}
		}()
	}
	start(primary, true)
	fallbackStarted := false
	startFallback := func() {
		if !fallbackStarted {
			fallbackStarted = true
			start(fallback, false)
		}
	}

	timer := time.NewTimer(d.fallbackDelay())
	defer timer.Stop()
	var primaryErr error
	for {
		select {
		case <-timer.C:
			startFallback()
		case r := <-results:
			pending--
			if r.err == nil {
				// Hand the loser its cancellation and close whatever it comes
				// back with. Both happen off this goroutine so a winner is not
				// held up by a fallback that is still connecting.
				go func(n int) {
					cancel()
					for i := 0; i < n; i++ {
						if o := <-results; o.conn != nil {
							_ = o.conn.Close()
						}
					}
				}(pending)
				return r.conn, nil
			}
			if r.primary {
				primaryErr = r.err
			}
			startFallback()
			if pending == 0 {
				cancel()
				if primaryErr != nil {
					return nil, primaryErr
				}
				return nil, r.err
			}
		}
	}
}

// dial is the one place a socket is opened. The dialler it uses carries no
// Control hook: the floor has already run, on the very address being dialled,
// and a second mechanism answering the same question is the drift this type
// exists to remove. The deadline is the context's, set by the caller.
func (d *Dialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.dialOne != nil {
		return d.dialOne(ctx, network, addr)
	}
	var base net.Dialer
	return base.DialContext(ctx, network, addr)
}

func (d *Dialer) allow(ctx context.Context, ip net.IP) error {
	if d.Allow != nil {
		return d.Allow(ctx, ip)
	}
	return IPAllowed(ip)
}

func (d *Dialer) lookup(ctx context.Context, network, host string) ([]net.IP, error) {
	lookup := d.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIP
	}
	return lookup(ctx, lookupNetwork(network), host)
}

func (d *Dialer) fallbackDelay() time.Duration {
	if d.FallbackDelay > 0 {
		return d.FallbackDelay
	}
	return defaultFallbackDelay
}

// lookupNetwork maps a dial network onto the spelling net.Resolver.LookupIP
// takes, so a "tcp4" dial resolves only A records exactly as net.Dialer's own
// resolution would. Anything else asks for both families, which is what "tcp",
// "udp" and the unix networks all want (a unix network never reaches here — it
// has no host:port to split).
func lookupNetwork(network string) string {
	switch network {
	case "tcp4", "udp4", "ip4":
		return "ip4"
	case "tcp6", "udp6", "ip6":
		return "ip6"
	default:
		return "ip"
	}
}

// partition splits the resolved list by address family, keeping the resolver's
// order within each half and making the family of the first address the primary
// one — the same rule net.Dialer uses, so the family a resolver preferred is
// still the family tried first.
func partition(addrs []netip.Addr) (primary, fallback []netip.Addr) {
	if len(addrs) == 0 {
		return nil, nil
	}
	is4 := addrs[0].Is4() || addrs[0].Is4In6()
	for _, a := range addrs {
		if a4 := a.Is4() || a.Is4In6(); a4 == is4 {
			primary = append(primary, a)
		} else {
			fallback = append(fallback, a)
		}
	}
	return primary, fallback
}

// partialDeadline gives one address its share of what is left, floored at
// minPartialTimeout so a long list does not reduce each attempt to a slice too
// short to complete a handshake in. With no deadline on ctx it returns ctx
// unchanged.
func partialDeadline(ctx context.Context, remaining int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || remaining <= 1 {
		return context.WithCancel(ctx)
	}
	left := time.Until(deadline)
	if left <= 0 {
		return context.WithCancel(ctx)
	}
	share := left / time.Duration(remaining)
	if share < minPartialTimeout {
		if left < minPartialTimeout {
			share = left
		} else {
			share = minPartialTimeout
		}
	}
	return context.WithTimeout(ctx, share)
}

// parseAddr reads an address literal, zone and all. It is netip rather than
// net.ParseIP because net.ParseIP answers nil for a zone-scoped address such as
// fe80::1%eth0, which a caller may legitimately dial and which must not be
// mistaken for a name to resolve.
func parseAddr(host string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a, true
}

// ipOf returns the address the floor judges. A zone names an interface rather
// than a destination, and IPAllowed asks only about the destination — which
// comes for free here: netip keeps a zone beside the address rather than in it,
// so AsSlice never carries one. Spelling that as a WithZone("") call would read
// like a guard while removing it changed nothing.
func ipOf(a netip.Addr) net.IP {
	return net.IP(a.AsSlice())
}

// joinAddrPort spells one resolved address for the dial, keeping any zone.
func joinAddrPort(a netip.Addr, port string) string {
	return net.JoinHostPort(a.String(), port)
}
