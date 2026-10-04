package sandboxtest

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// ExecLedger is the platform execs a fake sandbox refused for want of the
// script preamble (RefuseUnscripted). Its zero value is ready to use.
type ExecLedger struct {
	mu   sync.Mutex
	cmds []string
}

func (l *ExecLedger) add(cmd string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmds = append(l.cmds, cmd)
}

// All is every command l holds, in the order they were refused.
func (l *ExecLedger) All() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.cmds...)
}

// Report fails t once for every command l holds — a harness's check, at the
// end of a test, of the fake it handed out.
func (l *ExecLedger) Report(t testing.TB) {
	t.Helper()
	for _, cmd := range l.All() {
		t.Errorf("a platform exec without the script preamble (sandbox.Script): %q", cmd)
	}
}

// unscripted is every exec any fake in this test binary refused (Main).
var unscripted ExecLedger

// RefuseUnscripted is a fake sandbox's check of an exec it was handed: nil for
// one the platform may run (Scripted), and otherwise the error the fake
// answers it with, the command recorded in l — the fake's own, which its
// harness reports — and for the whole test binary, which Main fails on. A
// fake strips the preamble before it answers, so without this a platform exec
// that lost it would pass every unit test (#860).
func RefuseUnscripted(l *ExecLedger, command string) error {
	if Scripted(command) {
		return nil
	}
	l.add(command)
	unscripted.add(command)
	return fmt.Errorf("fake sandbox: a platform exec without the script preamble (sandbox.Script): %q", command)
}

// Main runs a test binary's tests through run — (*testing.M).Run, or a
// wrapper of it such as pgtest.Main — and fails the binary when any of its
// fakes refused an exec (RefuseUnscripted): the backstop for a fake no
// harness reports, whose refusal a best-effort caller may have swallowed.
func Main(m *testing.M, run func(*testing.M) int) int {
	code := run(m)
	if cmds := unscripted.All(); len(cmds) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d platform execs without the script preamble (sandbox.Script): %q\n", len(cmds), cmds)
		code = 1
	}
	return code
}
