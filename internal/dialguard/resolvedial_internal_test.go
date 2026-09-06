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

// ips builds a resolver answer. net.IPAddr rather than net.IP because that is
// what net.Resolver.LookupIPAddr returns and the only shape that can carry a
// zone; "addr%zone" spells one.
func ips(t *testing.T, list ...string) []net.IPAddr {
	t.Helper()
	out := make([]net.IPAddr, 0, len(list))
	for _, s := range list {
		host, zone, _ := strings.Cut(s, "%")
		ip := net.ParseIP(host)
		if ip == nil {
			t.Fatalf("test fixture %q is not an address", s)
		}
		out = append(out, net.IPAddr{IP: ip, Zone: zone})
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
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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
		Lookup:  func(context.Context, string) ([]net.IPAddr, error) { return nil, nil },
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
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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

// TestWithTheRaceOffTheResolversOrderIsKept: partitioning by family exists to
// race the two halves, so a caller that turned the race off must get the list
// as the resolver left it rather than one regrouped behind its back.
func TestWithTheRaceOffTheResolversOrderIsKept(t *testing.T) {
	t.Parallel()
	var rec recorder
	d := &Dialer{
		FallbackDelay: -1,
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return ips(t, "2001:db8::1", "198.51.100.1", "2001:db8::2"), nil
		},
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			return nil, errors.New("connection refused")
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "mixed.example:443"); err == nil {
		t.Fatal("every address refused, so the dial must fail")
	}
	want := []string{"[2001:db8::1]:443", "198.51.100.1:443", "[2001:db8::2]:443"}
	got := rec.all()
	if len(got) != len(want) {
		t.Fatalf("dialled %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dialled %v, want the resolver's order %v", got, want)
		}
	}

	// And the same for a network that is not "tcp": net.Dialer races the two
	// families for that network alone, so anything else is tried in the
	// resolver's order however long the fallback delay is.
	rec = recorder{}
	d.FallbackDelay = time.Second
	if _, err := d.DialContext(context.Background(), "udp", "mixed.example:443"); err == nil {
		t.Fatal("every address refused, so the dial must fail")
	}
	got = rec.all()
	if len(got) != len(want) {
		t.Fatalf("udp: dialled %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("udp: dialled %v, want the resolver's order %v — only tcp races", got, want)
		}
	}
}

// TestAFamilyWithNoSurvivorIsNotRacedAgainstTheOther: when the floor refuses
// every address of the family the resolver preferred, the other family is all
// there is, and racing an empty half against it would put "no address to dial"
// where the real connect error belongs.
func TestAFamilyWithNoSurvivorIsNotRacedAgainstTheOther(t *testing.T) {
	t.Parallel()
	connectErr := errors.New("connection refused by the origin")
	d := &Dialer{
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return ips(t, "::1", "::2", "198.51.100.1"), nil
		},
		Allow: func(_ context.Context, ip net.IP) error {
			if ip.String() == "::2" {
				return IPAllowed(net.IP{}) // refused, like ::1 before it
			}
			return IPAllowed(ip)
		},
		dialOne: func(context.Context, string, string) (net.Conn, error) { return nil, connectErr },
	}
	_, err := d.DialContext(context.Background(), "tcp", "dual.example:443")
	if !errors.Is(err, connectErr) {
		t.Fatalf("err = %v, want the surviving family's own connect error", err)
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
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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

// TestTheTimeoutBoundsTheWholeDial: net.Dialer's Timeout covers resolution as
// well as the connects, and so must this one — a resolver that never answers,
// or a literal that never connects, must not hold the caller for as long as its
// context allows. For the gate that context is the sandbox's own request, which
// is why its dialer carries a bound at all.
func TestTheTimeoutBoundsTheWholeDial(t *testing.T) {
	t.Parallel()
	const budget = 60 * time.Millisecond
	// How long the stand-in waits for a bound that should have arrived long
	// before. Comfortably over the budget and comfortably under the assertion.
	const giveUp = 2 * time.Second
	t.Run("a resolver that never answers", func(t *testing.T) {
		t.Parallel()
		d := &Dialer{
			Timeout: budget,
			Lookup: func(ctx context.Context, _ string) ([]net.IPAddr, error) {
				// Bounded, not blocking: a test that hangs when the bound goes
				// missing takes the whole package down with it and reports the
				// regression as a timeout rather than as itself.
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(giveUp):
					return ips(t, "198.51.100.1"), nil
				}
			},
			dialOne: func(context.Context, string, string) (net.Conn, error) {
				t.Error("the lookup never answered, so nothing may be dialled")
				return nil, errors.New("unreachable")
			},
		}
		start := time.Now()
		if _, err := d.DialContext(context.Background(), "tcp", "slow.example:443"); err == nil {
			t.Fatal("want the dial to give up")
		}
		if elapsed := time.Since(start); elapsed > giveUp {
			t.Errorf("returned in %v, want about the %v budget", elapsed, budget)
		}
	})
	t.Run("a literal that never connects", func(t *testing.T) {
		t.Parallel()
		d := &Dialer{
			Timeout: budget,
			dialOne: func(ctx context.Context, _, _ string) (net.Conn, error) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(giveUp):
					return stubConn(t), nil
				}
			},
		}
		start := time.Now()
		if _, err := d.DialContext(context.Background(), "tcp", "198.51.100.9:443"); err == nil {
			t.Fatal("want the dial to give up")
		}
		if elapsed := time.Since(start); elapsed > giveUp {
			t.Errorf("returned in %v, want about the %v budget", elapsed, budget)
		}
	})
}

// TestBothFamiliesFailingReportsThePrimarysError keeps the message naming the
// family the resolver preferred.
func TestBothFamiliesFailingReportsThePrimarysError(t *testing.T) {
	t.Parallel()
	primaryErr := errors.New("primary said no")
	d := &Dialer{
		FallbackDelay: time.Millisecond,
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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
				Lookup: func(context.Context, string) ([]net.IPAddr, error) {
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

// TestAnAddressTheFloorCannotReadKeepsItsOldAnswer. Three authorities carry no
// address the floor can read: a zone-scoped literal, the empty host of ":443",
// and a bracketed host that is not an address at all. None of them could reach
// a socket for a floored class before — the first two errored inside the
// Control hook before its predicate ran, and the third failed in the resolver
// without the hook being reached — and all of them went to the standard dialler
// unchanged for a class the gate exempts from the floor, which had no hook
// installed. Both halves have to survive, or the change moves what a session
// can reach. What does change for the floored half is the error's kind: the
// third used to fail as a lookup and now fails as ErrRefused.
func TestAnAddressTheFloorCannotReadKeepsItsOldAnswer(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"[2001:db8::1%eth0]:443",
		"[fe80::1%eth0]:443",
		":443",
		// The last one is the only shape here that reaches the unreadable
		// branch by its colon alone: the other three carry a percent sign or
		// an empty host. Without it the colon is a guard no test can fail.
		"[foo:bar]:443",
	} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			var judged []net.IP
			var rec recorder
			floored := &Dialer{
				Allow: func(_ context.Context, ip net.IP) error {
					judged = append(judged, ip)
					return IPAllowed(ip)
				},
				Lookup: func(context.Context, string) ([]net.IPAddr, error) {
					t.Error("this is not a name and must not be resolved")
					return nil, errors.New("unreachable")
				},
				dialOne: func(context.Context, string, string) (net.Conn, error) {
					t.Error("a floored class refused this before and must still")
					return nil, errors.New("unreachable")
				},
			}
			if _, err := floored.DialContext(context.Background(), "tcp", addr); !errors.Is(err, ErrRefused) {
				t.Fatalf("floored: err = %v, want ErrRefused", err)
			}
			if len(judged) != 1 || judged[0] != nil {
				t.Errorf("the floor was asked about %v, want one unreadable address", judged)
			}

			// The gate's own shape for a class it exempts: Allow answers before
			// it looks at the address at all.
			exempt := &Dialer{
				Allow:   func(context.Context, net.IP) error { return nil },
				dialOne: func(_ context.Context, _, a string) (net.Conn, error) { rec.add(a); return stubConn(t), nil },
			}
			c, err := exempt.DialContext(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatalf("exempt: %v — an unfloored class dialled this unchanged before", err)
			}
			_ = c.Close()
			if got := rec.all(); len(got) != 1 || got[0] != addr {
				t.Errorf("dialled %v, want %q spelled exactly as it came", got, addr)
			}
		})
	}
}

// TestANetworkWithNoPortIsRefused. This type takes a name and a port apart,
// judges the addresses and dials them; a raw "ip:proto" address is a bare host
// and a unix address is a path, so neither has anything for the floor to be
// asked about. The refusal is the type's own rather than the floor's, which is
// what this drives: it holds even for a caller that exempts the class from the
// floor entirely — the gate's `allowed_hosts` shape — where otherwise a socket
// would open on an address nothing judged. A network that merely looks like one
// must not cost a real lookup either.
func TestANetworkWithNoPortIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ network, addr string }{
		{"ip:icmp", "host.example"},
		{"ip4:1", "198.51.100.1"},
		{"unix", "/tmp/sock"},
		{"unixgram", "/tmp/sock"},
		{"tcp1", "host.example:443"},
		{"tcpmux", "host.example:443"},
	} {
		d := &Dialer{
			// The exempting shape: this caller's floor admits everything, so
			// only portCarrying can refuse.
			Allow: func(context.Context, net.IP) error { return nil },
			Lookup: func(context.Context, string) ([]net.IPAddr, error) {
				t.Errorf("%s: spent a lookup on a network with no address to judge", tc.network)
				return nil, errors.New("unreachable")
			},
			dialOne: func(context.Context, string, string) (net.Conn, error) {
				t.Errorf("%s: dialled without the floor", tc.network)
				return nil, errors.New("unreachable")
			},
		}
		if _, err := d.DialContext(context.Background(), tc.network, tc.addr); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", tc.network, err)
		}
	}
	// And the six that do carry a port are not caught by it.
	for _, network := range []string{"tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"} {
		if !portCarrying(network) {
			t.Errorf("portCarrying(%q) = false, want true", network)
		}
	}
}

// TestANegativeTimeoutIsAlreadyExpired: net.Dialer reads a negative Timeout as a
// deadline in the past rather than as no deadline (dial.go's deadline method
// tests Timeout != 0), and a drop-in that read it as "unbounded" would turn a
// caller's mistake into an unbounded dial.
func TestANegativeTimeoutIsAlreadyExpired(t *testing.T) {
	t.Parallel()
	d := &Dialer{
		Timeout: -time.Second,
		Lookup: func(ctx context.Context, _ string) ([]net.IPAddr, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return ips(t, "198.51.100.1"), nil
		},
		dialOne: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return stubConn(t), nil
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "host.example:443"); err == nil {
		t.Fatal("a negative timeout is already expired, so the dial must fail")
	}
}

// TestTheResolversPreferredFamilyLeadsThroughARefusal: which family races first
// is decided by the whole answer, and the floor refusing that family's first
// address must not hand the lead to the other one. net.Dialer partitions before
// its Control hook runs, so this is fidelity rather than taste — and getting it
// wrong picks a different backend and a different latency profile on every
// dual-stack name with one blocked address.
func TestTheResolversPreferredFamilyLeadsThroughARefusal(t *testing.T) {
	t.Parallel()
	var rec recorder
	d := &Dialer{
		FallbackDelay: 5 * time.Second, // long enough that the fallback cannot win
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			// v6 leads, but the resolver's first address is one the floor
			// refuses; a second v6 address survives.
			return ips(t, "::1", "198.51.100.1", "2001:db8::2"), nil
		},
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			return stubConn(t), nil
		},
	}
	c, err := d.DialContext(context.Background(), "tcp", "dual.example:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if got := rec.all(); len(got) != 1 || got[0] != "[2001:db8::2]:443" {
		t.Errorf("dialled %v, want the surviving address of the resolver's own first family", got)
	}
}

// TestTheLosingFamilysConnectionIsClosed: the race's most important guarantee
// is not that a winner is returned but that the loser does not leak a socket
// when it comes up a moment later.
func TestTheLosingFamilysConnectionIsClosed(t *testing.T) {
	t.Parallel()
	loser := make(chan net.Conn, 1)
	closed := make(chan struct{})
	d := &Dialer{
		FallbackDelay: time.Millisecond,
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return ips(t, "2001:db8::1", "198.51.100.2"), nil
		},
		dialOne: func(ctx context.Context, _, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "[2001:db8::1]") {
				// The primary comes up late — after the fallback has won.
				<-closed
				c := stubConn(t)
				loser <- c
				return c, nil
			}
			return stubConn(t), nil
		},
	}
	c, err := d.DialContext(context.Background(), "tcp", "dual.example:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	close(closed) // let the loser finish now that a winner is in hand
	late := <-loser
	// A net.Pipe end reports use of a closed connection on a read after Close.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = late.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if _, err := late.Read(make([]byte, 1)); err != nil && strings.Contains(err.Error(), "closed") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the losing family's connection was never closed")
		}
	}
}

// TestAnAddressThisCannotSplitFailsAsBefore: a malformed authority is a
// caller's input rather than a destination, and must fail with the standard
// dialler's own error rather than becoming a refusal or a lookup.
func TestAnAddressThisCannotSplitFailsAsBefore(t *testing.T) {
	t.Parallel()
	// An authority this cannot take apart is judged like any other address it
	// cannot read, so a floored class refuses it and never opens a socket —
	// which is what the Control hook did, and which is the difference between
	// "nothing reaches the network unjudged" and "nothing anybody thought of
	// does".
	var rec recorder
	floored := &Dialer{
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			t.Error("an unsplittable address must not be resolved")
			return nil, errors.New("unreachable")
		},
		dialOne: func(context.Context, string, string) (net.Conn, error) {
			t.Error("a floored class must not reach the network with this")
			return nil, errors.New("unreachable")
		},
	}
	if _, err := floored.DialContext(context.Background(), "tcp", "[[::1]]:443"); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}

	// And a class the caller exempts still hands it on as it came, so it fails
	// with the standard dialler's own error as it did before this type existed.
	exempt := &Dialer{
		Allow: func(context.Context, net.IP) error { return nil },
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			rec.add(addr)
			return nil, errors.New("address error")
		},
	}
	if _, err := exempt.DialContext(context.Background(), "tcp", "[[::1]]:443"); err == nil {
		t.Fatal("want the dialler's own error")
	}
	if got := rec.all(); len(got) != 1 || got[0] != "[[::1]]:443" {
		t.Errorf("dialled %v, want the address handed on as it came", got)
	}
}

// TestTheDialNetworkFiltersTheFamilies: a "tcp4" dial takes A records only.
// net.Dialer both hints the resolver by family and filters what comes back;
// this reproduces the filtering half alone, because the network is not in a
// signature that has to keep the zone, and filtering is the half that decides
// what gets dialled.
func TestTheDialNetworkFiltersTheFamilies(t *testing.T) {
	t.Parallel()
	for network, want := range map[string][]string{
		"tcp":  {"2001:db8::1", "198.51.100.1"},
		"tcp4": {"198.51.100.1"},
		"tcp6": {"2001:db8::1"},
		"udp4": {"198.51.100.1"},
		"udp":  {"2001:db8::1", "198.51.100.1"},
	} {
		got, err := addrsOf(ips(t, "2001:db8::1", "198.51.100.1"), network)
		if err != nil {
			t.Errorf("%s: %v", network, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", network, got, want)
			continue
		}
		for i := range want {
			if got[i].String() != want[i] {
				t.Errorf("%s: got %v, want %v", network, got, want)
				break
			}
		}
	}
	// A network whose family the answer does not have is net.Dialer's "no
	// suitable address", not a dial to the wrong family.
	if _, err := addrsOf(ips(t, "198.51.100.1"), "tcp6"); !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v, want ErrRefused when no answer has the wanted family", err)
	}
}

// TestAResolvedZoneSurvivesToTheDial: net.Dialer resolves through LookupIPAddr
// because LookupIP drops IPAddr.Zone (lookup.go builds its result from each
// answer's IP alone). A name whose answer is zone-scoped must still be dialled
// with the zone that makes it routable — while the floor, which is asked about
// destinations rather than interfaces, sees the address without it.
func TestAResolvedZoneSurvivesToTheDial(t *testing.T) {
	t.Parallel()
	var judged []string
	var rec recorder
	d := &Dialer{
		Lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return ips(t, "2001:db8::1%eth0"), nil
		},
		Allow: func(_ context.Context, ip net.IP) error {
			judged = append(judged, ip.String())
			return nil
		},
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) { rec.add(addr); return stubConn(t), nil },
	}
	c, err := d.DialContext(context.Background(), "tcp", "zoned.example:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if len(judged) != 1 || judged[0] != "2001:db8::1" {
		t.Errorf("the floor judged %v, want the address without its zone", judged)
	}
	if got := rec.all(); len(got) != 1 || got[0] != "[2001:db8::1%eth0]:443" {
		t.Errorf("dialled %v, want the zone kept", got)
	}
}

// TestTheProductionResolverIsWiredUp. Every other test substitutes Lookup, so
// nothing else exercises the line that selects net.DefaultResolver.LookupIPAddr
// — and "resolves the name once" is this change's headline claim, so leaving
// the production path unexercised is the silent-skip shape the repo's mutation
// rule exists to catch. `localhost` answers from the hosts file, needs no
// network, and lands on an address the floor refuses, so the refusal proves the
// whole chain ran: default resolver, family filter, floor, and the address in
// the message.
func TestTheProductionResolverIsWiredUp(t *testing.T) {
	t.Parallel()
	d := &Dialer{
		Timeout: 5 * time.Second,
		dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
			t.Errorf("localhost resolves to loopback, which the floor refuses; dialled %q", addr)
			return nil, errors.New("unreachable")
		},
	}
	_, err := d.DialContext(context.Background(), "tcp", "localhost:443")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want the floor's refusal through the real resolver", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") && !strings.Contains(err.Error(), "::1") {
		t.Errorf("err = %v, want it to name the loopback address it refused", err)
	}
}

// TestTheBudgetsOwnErrorSurvives: when the budget is gone, net.Dialer answers
// with the context's error rather than with whatever the first address happened
// to fail with, so errors.Is(err, context.DeadlineExceeded) means what it says.
//
// Both halves are driven by a budget that is already spent rather than by one
// running out mid-dial. A budget under partialDeadline's two-second floor gives
// each address everything that is left, so the per-address deadline lands on
// the parent's and which timer fires first is not decided — and when the child
// wins, one more address is tried and its predecessor's error is what comes
// back, exactly as net.Dialer answers the same tie. The guard is what this
// holds; the tie is not a property either dialler has.
func TestTheBudgetsOwnErrorSurvives(t *testing.T) {
	t.Parallel()

	t.Run("spent before the first address", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		d := &Dialer{
			Lookup: func(context.Context, string) ([]net.IPAddr, error) {
				return ips(t, "198.51.100.1", "198.51.100.2"), nil
			},
			dialOne: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("connection refused")
			},
		}
		_, err := d.DialContext(ctx, "tcp", "slow.example:443")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the budget's own error", err)
		}
	})

	t.Run("spent between two addresses", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var rec recorder
		d := &Dialer{
			Lookup: func(context.Context, string) ([]net.IPAddr, error) {
				return ips(t, "198.51.100.1", "198.51.100.2"), nil
			},
			dialOne: func(_ context.Context, _, addr string) (net.Conn, error) {
				rec.add(addr)
				// The first attempt is what spends the budget, which is the
				// shape a real timeout has without the timer that makes it one.
				cancel()
				return nil, errors.New("connection refused")
			},
		}
		_, err := d.DialContext(ctx, "tcp", "slow.example:443")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the context's own error", err)
		}
		if got := rec.all(); len(got) != 1 {
			t.Errorf("dialled %v, want the second address left alone once there was no budget for it", got)
		}
	})
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
	primary, fallback := partition(is4(addrs[0]), addrs)
	if len(primary) != 2 || primary[0].String() != "2001:db8::1" || primary[1].String() != "2001:db8::2" {
		t.Errorf("primary = %v, want both v6 addresses in resolver order", primary)
	}
	if len(fallback) != 2 || fallback[0].String() != "198.51.100.1" {
		t.Errorf("fallback = %v, want both v4 addresses in resolver order", fallback)
	}
	if _, fb := partition(is4(addrs[1]), addrs[1:2]); len(fb) != 0 {
		t.Error("a single-family answer has no fallback half to race")
	}
	// An A record reaches this as an IPv4-mapped address, which is the form
	// net.Resolver.LookupIP returns; reading that as IPv6 would split one family
	// in two and race a name against itself.
	if !is4(netip.MustParseAddr("::ffff:198.51.100.1")) {
		t.Error("an IPv4-mapped address must count as IPv4, as net.Dialer counts it")
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
