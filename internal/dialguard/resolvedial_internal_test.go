package dialguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubConn is one end of a net.Pipe, which is a real net.Conn with no network
// under it — enough to tell "this dial answered" from "this dial did not".
func stubConn(t *testing.T) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a
}

// recorder captures what the dialler asked the network for.
type recorder struct {
	mu     sync.Mutex
	dialed []string
}

func (r *recorder) add(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialed = append(r.dialed, addr)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dialed...)
}

func ips(t *testing.T, list ...string) []net.IP {
	t.Helper()
	out := make([]net.IP, 0, len(list))
	for _, s := range list {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test fixture %q is not an address", s)
		}
		out = append(out, ip)
	}
	return out
}

// TestANameIsResolvedExactlyOncePerDial is the invariant the whole type exists
// for: whatever the dial does afterwards — one address, a failover, a race
// between two families — the name is looked up once and every attempt is made
// against that one answer.
func TestANameIsResolvedExactlyOncePerDial(t *testing.T) {
	t.Parallel()
	var lookups int
	var rec recorder
	d := &Dialer{
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			lookups++
			return ips(t, "198.51.100.1", "198.51.100.2"), nil
		},
		Allow: func(context.Context, net.IP) error { return nil },
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			return nil, errors.New("refused")
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "host.example:443"); err == nil {
		t.Fatal("every address refused the connection, so the dial must fail")
	}
	if lookups != 1 {
		t.Errorf("resolved %d times, want exactly 1 — the address the policy judged must be the address dialled", lookups)
	}
	if got := rec.all(); len(got) != 2 {
		t.Errorf("dialled %v, want both resolved addresses tried", got)
	}
}

// TestEveryResolvedAddressIsJudgedBeforeAnyConnect is the floor's half: it runs
// over the whole answer, and a refused address is never handed to the network at
// all rather than being stopped one syscall later.
func TestEveryResolvedAddressIsJudgedBeforeAnyConnect(t *testing.T) {
	t.Parallel()
	var judged []string
	var rec recorder
	d := &Dialer{
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			return ips(t, "127.0.0.1", "198.51.100.7"), nil
		},
		Allow: func(_ context.Context, ip net.IP) error {
			judged = append(judged, ip.String())
			return IPAllowed(ip)
		},
		dialOne: func(t2 context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			return stubConn(t), nil
		},
	}
	c, err := d.DialContext(context.Background(), "tcp", "host.example:443")
	if err != nil {
		t.Fatalf("the admitted address should still have been reachable: %v", err)
	}
	_ = c.Close()
	if want := []string{"127.0.0.1", "198.51.100.7"}; len(judged) != 2 || judged[0] != want[0] || judged[1] != want[1] {
		t.Errorf("judged %v, want every resolved address %v", judged, want)
	}
	if got := rec.all(); len(got) != 1 || got[0] != "198.51.100.7:443" {
		t.Errorf("dialled %v, want only the admitted address — a refused one must never reach the network", got)
	}
}

// TestARefusalSurvivesAsErrRefused keeps the sentinel vaultresolve matches on:
// a destination that can never be dialled must stay distinguishable from a
// network that may recover.
func TestARefusalSurvivesAsErrRefused(t *testing.T) {
	t.Parallel()
	d := &Dialer{
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			return ips(t, "127.0.0.1", "169.254.169.254"), nil
		},
		dialOne: func(context.Context, string, string) (net.Conn, error) {
			t.Error("no address survived the floor, so nothing may be dialled")
			return nil, errors.New("unreachable")
		},
	}
	_, err := d.DialContext(context.Background(), "tcp", "metadata.example:80")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want it to wrap ErrRefused", err)
	}
	// And it must be *the* refusal rather than a summary of there having been
	// one: IPAllowed names the address it refused, which is the only thing
	// telling an operator which of a name's addresses is the problem.
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("err = %v, want the first refusal, which names the address", err)
	}
}

// TestAnAnswerWithNoAddressIsRefused covers the seam's own edge: a resolver
// that answers with neither addresses nor an error must not fall through to a
// dial with nothing to dial.
func TestAnAnswerWithNoAddressIsRefused(t *testing.T) {
	t.Parallel()
	d := &Dialer{
		Lookup:  func(context.Context, string, string) ([]net.IP, error) { return nil, nil },
		dialOne: func(context.Context, string, string) (net.Conn, error) { t.Error("nothing to dial"); return nil, nil },
	}
	if _, err := d.DialContext(context.Background(), "tcp", "empty.example:80"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

// TestAFailedAddressFallsOverToTheNext is one of the three things net.Dialer
// does with a resolved list that a bare literal dial does not.
func TestAFailedAddressFallsOverToTheNext(t *testing.T) {
	t.Parallel()
	var rec recorder
	d := &Dialer{
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			return ips(t, "198.51.100.1", "198.51.100.2"), nil
		},
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			if strings.HasPrefix(addr, "198.51.100.1:") {
				return nil, errors.New("connection refused")
			}
			return stubConn(t), nil
		},
	}
	c, err := d.DialContext(context.Background(), "tcp", "host.example:443")
	if err != nil {
		t.Fatalf("the second address answered, so the dial must succeed: %v", err)
	}
	_ = c.Close()
	if got := rec.all(); len(got) != 2 || got[1] != "198.51.100.2:443" {
		t.Errorf("dialled %v, want the first tried then the second", got)
	}
}

// TestTheOtherFamilyStartsAfterTheFallbackDelay is Happy Eyeballs: a blackholed
// primary family must cost the delay, not the whole timeout. Driven through the
// dial seam rather than the network, so it neither needs IPv6 nor depends on
// how a machine fails an unreachable address.
func TestTheOtherFamilyStartsAfterTheFallbackDelay(t *testing.T) {
	t.Parallel()
	const delay = 40 * time.Millisecond
	d := &Dialer{
		Timeout:       10 * time.Second,
		FallbackDelay: delay,
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			return ips(t, "2001:db8::1", "198.51.100.2"), nil
		},
		dialOne: func(ctx context.Context, _, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "[2001:db8::1]") {
				<-ctx.Done() // a blackhole: no answer, no refusal
				return nil, ctx.Err()
			}
			return stubConn(t), nil
		},
	}
	start := time.Now()
	c, err := d.DialContext(context.Background(), "tcp", "dual.example:443")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the second family answered, so the dial must succeed: %v", err)
	}
	_ = c.Close()
	if elapsed < delay {
		t.Errorf("returned in %v, before the fallback delay %v — the second family must not start early", elapsed, delay)
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned in %v, want about the fallback delay — a blackholed family must not cost the timeout", elapsed)
	}
}

// TestBothFamiliesFailingReportsThePrimarysError keeps the message naming the
// family the resolver preferred.
func TestBothFamiliesFailingReportsThePrimarysError(t *testing.T) {
	t.Parallel()
	primaryErr := errors.New("primary said no")
	d := &Dialer{
		FallbackDelay: time.Millisecond,
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			return ips(t, "2001:db8::1", "198.51.100.2"), nil
		},
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "[2001:db8::1]") {
				return nil, primaryErr
			}
			return nil, errors.New("fallback said no")
		},
	}
	_, err := d.DialContext(context.Background(), "tcp", "dual.example:443")
	if !errors.Is(err, primaryErr) {
		t.Fatalf("err = %v, want the primary family's error", err)
	}
}

// TestALiteralIsNotResolved: an address literal is a destination, not a name,
// and resolving one would be a different operation with different failures.
func TestALiteralIsNotResolved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		addr    string
		refused bool
		dialed  string
	}{
		{name: "IPv4", addr: "198.51.100.9:443", dialed: "198.51.100.9:443"},
		{name: "IPv6", addr: "[2001:db8::5]:443", dialed: "[2001:db8::5]:443"},
		{name: "a refused IPv4 literal", addr: "127.0.0.1:443", refused: true},
		{name: "a refused IPv6 literal", addr: "[::1]:443", refused: true},
		{name: "a zone-scoped literal keeps its zone", addr: "[fe80::1%eth0]:443", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec recorder
			d := &Dialer{
				Lookup: func(context.Context, string, string) ([]net.IP, error) {
					t.Error("a literal must not be resolved")
					return nil, errors.New("unreachable")
				},
				dialOne: func(_ context.Context, _, addr string) (net.Conn, error) { rec.add(addr); return stubConn(t), nil },
			}
			c, err := d.DialContext(context.Background(), "tcp", tc.addr)
			if tc.refused {
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("err = %v, want ErrRefused", err)
				}
				if got := rec.all(); len(got) != 0 {
					t.Errorf("dialled %v, want nothing", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			_ = c.Close()
			if got := rec.all(); len(got) != 1 || got[0] != tc.dialed {
				t.Errorf("dialled %v, want %q spelled exactly as it came", got, tc.dialed)
			}
		})
	}
}

// TestAZoneScopedLiteralIsJudgedWithoutItsZone: a zone names an interface, not a
// destination, so the floor must see the address alone — while the dial keeps
// the zone, which is the whole reason the address was written that way.
func TestAZoneScopedLiteralIsJudgedWithoutItsZone(t *testing.T) {
	t.Parallel()
	var judged string
	var rec recorder
	d := &Dialer{
		Allow:   func(_ context.Context, ip net.IP) error { judged = ip.String(); return nil },
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) { rec.add(addr); return stubConn(t), nil },
	}
	c, err := d.DialContext(context.Background(), "tcp", "[2001:db8::1%eth0]:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if judged != "2001:db8::1" {
		t.Errorf("the floor judged %q, want the address without its zone", judged)
	}
	if got := rec.all(); len(got) != 1 || got[0] != "[2001:db8::1%eth0]:443" {
		t.Errorf("dialled %v, want the zone kept", got)
	}
}

// TestAnAddressThisCannotSplitFailsAsBefore: a malformed authority is a
// caller's input rather than a destination, and must fail with the standard
// dialler's own error rather than becoming a refusal or a lookup.
func TestAnAddressThisCannotSplitFailsAsBefore(t *testing.T) {
	t.Parallel()
	var rec recorder
	d := &Dialer{
		Lookup: func(context.Context, string, string) ([]net.IP, error) {
			t.Error("an unsplittable address must not be resolved")
			return nil, errors.New("unreachable")
		},
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			return nil, errors.New("address error")
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "[[::1]]:443"); err == nil {
		t.Fatal("want the dialler's own error")
	}
	if got := rec.all(); len(got) != 1 || got[0] != "[[::1]]:443" {
		t.Errorf("dialled %v, want the address handed on as it came", got)
	}
}

// TestTheLookupFollowsTheDialNetwork: a "tcp4" dial must resolve only A
// records, exactly as net.Dialer's own resolution would.
func TestTheLookupFollowsTheDialNetwork(t *testing.T) {
	t.Parallel()
	for network, want := range map[string]string{
		"tcp": "ip", "tcp4": "ip4", "tcp6": "ip6", "udp4": "ip4", "": "ip",
	} {
		if got := lookupNetwork(network); got != want {
			t.Errorf("lookupNetwork(%q) = %q, want %q", network, got, want)
		}
	}
	var asked string
	d := &Dialer{
		Lookup: func(_ context.Context, network, _ string) ([]net.IP, error) {
			asked = network
			return ips(t, "198.51.100.1"), nil
		},
		dialOne: func(context.Context, string, string) (net.Conn, error) { return stubConn(t), nil },
	}
	c, err := d.DialContext(context.Background(), "tcp4", "host.example:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if asked != "ip4" {
		t.Errorf("resolved for %q, want ip4", asked)
	}
}

// TestPartitionPrefersTheFamilyTheResolverPutFirst keeps the resolver's RFC 6724
// ordering meaningful: it decides which family races first, and order within a
// family is untouched.
func TestPartitionPrefersTheFamilyTheResolverPutFirst(t *testing.T) {
	t.Parallel()
	addrs := []netip.Addr{
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("198.51.100.1"),
		netip.MustParseAddr("2001:db8::2"),
		netip.MustParseAddr("198.51.100.2"),
	}
	primary, fallback := partition(addrs)
	if len(primary) != 2 || primary[0].String() != "2001:db8::1" || primary[1].String() != "2001:db8::2" {
		t.Errorf("primary = %v, want both v6 addresses in resolver order", primary)
	}
	if len(fallback) != 2 || fallback[0].String() != "198.51.100.1" {
		t.Errorf("fallback = %v, want both v4 addresses in resolver order", fallback)
	}
	if _, fb := partition(addrs[1:2]); len(fb) != 0 {
		t.Error("a single-family answer has no fallback half to race")
	}
}

// TestEachAddressGetsAShareOfTheBudget: one address must not be able to spend
// the whole budget, or a name's later addresses are unreachable in practice.
// The share is net.Dialer's own rule, floor included — with less than the floor
// left, the next attempt gets what remains rather than a slice too short to
// complete a handshake in.
func TestEachAddressGetsAShareOfTheBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		budget    time.Duration
		remaining int
		want      time.Duration
	}{
		{name: "four addresses split a comfortable budget", budget: 10 * time.Second, remaining: 4, want: 2500 * time.Millisecond},
		{name: "a long list is floored at the sane minimum", budget: 10 * time.Second, remaining: 10, want: minPartialTimeout},
		{name: "under the floor, the attempt gets what is left", budget: time.Second, remaining: 10, want: time.Second},
		{name: "the last address gets the whole remainder", budget: 10 * time.Second, remaining: 1, want: 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
			defer cancel()
			actx, acancel := partialDeadline(ctx, tc.remaining)
			defer acancel()
			deadline, ok := actx.Deadline()
			if !ok {
				t.Fatal("want a deadline on the per-address context")
			}
			if got := time.Until(deadline); got > tc.want || got < tc.want-time.Second {
				t.Errorf("share = %v, want about %v", got, tc.want)
			}
		})
	}
	if _, ok := func() (context.Context, bool) {
		c, cancel := partialDeadline(context.Background(), 4)
		defer cancel()
		_, ok := c.Deadline()
		return c, ok
	}(); ok {
		t.Error("with no budget there is nothing to share, so no per-address deadline")
	}
}
