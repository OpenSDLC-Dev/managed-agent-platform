package api

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

var allCursorLists = []cursorList{
	cursorAgents, cursorAgentVersions, cursorDeployments, cursorDeploymentRuns, cursorDreams,
	cursorEnvironments, cursorFiles, cursorMemories, cursorMemoryStores, cursorMemoryVersions,
	cursorServiceAccounts, cursorSessions, cursorSessionEvents, cursorSkills, cursorSkillVersions,
	cursorThreads, cursorThreadEvents, cursorVaults, cursorVaultCredentials, cursorWork,
}

func FuzzDecodeCursorNoPanic(f *testing.F) {
	now := time.Unix(1_700_000_000, 123456789).UTC()
	f.Add("")
	f.Add("not-base64!!!")
	f.Add(encodeTimeCursor("default", cursorAgents, dirNext, now, string(domain.NewID(domain.PrefixAgent))))
	f.Add(encodeVersionCursor("default", 7))
	f.Add(encodePathCursor("default", "/notes/a|b/file.md"))
	f.Add(encodeSeqCursor("default", cursorSessionEvents, domain.NewID(domain.PrefixSessionThread), true, domain.Event{
		ID:  domain.NewID(domain.PrefixEvent),
		Seq: 12,
	}))

	f.Fuzz(func(t *testing.T, token string) {
		c, err := decodeCursor(token)
		if err != nil {
			return
		}
		if !c.list.valid() {
			t.Fatalf("decodeCursor(%q) returned invalid list %q", token, c.list)
		}
		if c.dir != dirNext && c.dir != dirPrev {
			t.Fatalf("decodeCursor(%q) returned invalid dir %q", token, c.dir)
		}
		// Exercise the real list-binding guard (foreignToTime) across all lists:
		// a time cursor must be accepted on its own list and rejected on every
		// other list; non-time cursors must be rejected by every time-keyed list.
		timeKeyed := !c.versioned && !c.seqKeyed && !c.pathKeyed
		for _, target := range allCursorLists {
			gotForeign := c.foreignToTime(target)
			wantForeign := !timeKeyed || target != c.list
			if gotForeign != wantForeign {
				t.Fatalf("foreignToTime(target=%q) for cursor(list=%q, timeKeyed=%v) = %v, want %v",
					target, c.list, timeKeyed, gotForeign, wantForeign)
			}
		}
	})
}

func FuzzTimeCursorRoundTrip(f *testing.F) {
	f.Add(uint8(0), true, int64(1_700_000_000_000_000_000), "agent_0123456789abcdefghjkmnpq")
	f.Add(uint8(5), false, int64(0), "sesn_0123456789abcdefghjkmnpq")
	f.Add(uint8(13), true, int64(-123456789), "xlsx")

	f.Fuzz(func(t *testing.T, listIdx uint8, next bool, unixNano int64, id string) {
		if id == "" || strings.Contains(id, "|") {
			return
		}
		list := allCursorLists[int(listIdx)%len(allCursorLists)]
		dir := dirPrev
		if next {
			dir = dirNext
		}
		ts := time.Unix(0, unixNano).UTC()
		tok := encodeTimeCursor("default", list, dir, ts, id)
		got, err := decodeCursor(tok)
		if err != nil {
			t.Fatalf("decodeCursor(encodeTimeCursor(%q,%q,%d,%q)) failed: %v", list, dir, unixNano, id, err)
		}
		if got.list != list || got.dir != dir || got.t.UnixNano() != ts.UnixNano() || got.id != id {
			t.Fatalf("round-trip mismatch: got (%q,%q,%d,%q), want (%q,%q,%d,%q)",
				got.list, got.dir, got.t.UnixNano(), got.id, list, dir, ts.UnixNano(), id)
		}
	})
}

func FuzzPathCursorRoundTrip(f *testing.F) {
	for _, seed := range []string{
		"/a",
		"/notes/a|b|c/file.md",
		"/path with spaces/'quotes'/中文.txt",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		if path == "" || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
			return
		}
		tok := encodePathCursor("default", path)
		got, err := decodeCursor(tok)
		if err != nil {
			t.Fatalf("decodeCursor(encodePathCursor(%q)) failed: %v", path, err)
		}
		if !got.pathKeyed || got.list != cursorMemories || got.path != path {
			t.Fatalf("path cursor round-trip mismatch: got %+v, want path=%q", got, path)
		}
	})
}

func FuzzSeqCursorRoundTrip(f *testing.F) {
	f.Add(true, int64(1), true)
	f.Add(false, int64(999999), false)

	f.Fuzz(func(t *testing.T, threadList bool, seq int64, desc bool) {
		if seq < 1 {
			return
		}
		list := cursorSessionEvents
		if threadList {
			list = cursorThreadEvents
		}
		tid := domain.NewID(domain.PrefixSessionThread)
		eid := domain.NewID(domain.PrefixEvent)
		tok := encodeSeqCursor("default", list, tid, desc, domain.Event{ID: eid, Seq: seq})
		got, err := decodeCursor(tok)
		if err != nil {
			t.Fatalf("decodeCursor(encodeSeqCursor) failed: %v", err)
		}
		if !got.seqKeyed || got.list != list || got.seqDesc != desc || got.seq != seq || got.threadID != tid || got.eventID != eid {
			t.Fatalf("seq cursor round-trip mismatch: got %+v, want list=%q desc=%v seq=%d tid=%q eid=%q",
				got, list, desc, seq, tid, eid)
		}
	})
}
