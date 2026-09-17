package main

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Rungs 2 and 3 — resolution, and the report.
//
// The split between them is not fastidiousness, it is what keeps the gate
// offline and honest. Rung 2 may fail, so it may only ask questions the gate can
// always answer: the pinned module is in the build graph and therefore already
// on disk, while the registry cites tags no cold runner holds. Rung 3 asks the
// question that actually matters — has anything the corpus cites moved at the
// pin? — and never fails the gate, because at gate time a vanished symbol is not
// yet a defect. The entry may be describing a version where it existed. A gate that
// reddened until every such claim was re-verified would convert "re-check what
// changed" back into "edit everything now", which is the disease. The pull
// request moving a pin is where a transition is read instead, and there one
// nobody has dispositioned fails -report.
//
// Rung 3 is also the only rung that may read a tag other than the pin, and only
// ever one the module cache already holds. That is where a span's reason gets
// falsified against the tag it was actually written for: available, never
// required, so `make verify` never passes or fails by accident of a disk.

// modules maps a source name to the Go module that carries it. anthropic-cli is
// deliberately absent: it is a tagged local checkout, not a dependency, so CI
// cannot resolve its symbols at all and its citations get rung 1 only.
var modules = map[string]string{
	"anthropic-sdk-go": SDKModule,
	"go-jose":          "github.com/go-jose/go-jose/v4",
}

// Env holds what the resolving rungs need, resolved once.
type Env struct {
	Pin         string // the version go.mod pins for the SDK
	resolvers   map[string]*Resolver
	versions    map[string]string
	spec        *Spec
	unreachable map[string]string // source -> why it could not be resolved
	replaced    map[string]string // source -> the caveat saying what tree stands in for its tag
	caveats     []string          // things true of this run that change what it means
	older       map[string]*Resolver
	olderErr    map[string]string
	// cache finds a module version already unpacked on this machine. It is a
	// field so a test can decide what the cache holds: "reported when
	// available" is behaviour whose two branches both have to be exercised, and
	// neither may depend on which tags a developer's disk happens to carry.
	cache func(modulePath, version string) (string, error)
}

// NewEnv resolves every module in the grammar's scope, offline. A module it
// cannot reach is recorded rather than fatal: its citations become uncheckable,
// which the report names, instead of vanishing from a run that then looks clean.
func NewEnv(repoRoot string) (*Env, error) {
	e := &Env{
		resolvers:   map[string]*Resolver{},
		versions:    map[string]string{},
		unreachable: map[string]string{},
		replaced:    map[string]string{},
		older:       map[string]*Resolver{},
		olderErr:    map[string]string{},
		cache:       CachedModule,
	}
	for source, path := range modules {
		mod, err := Module(repoRoot, path)
		if err != nil {
			e.unreachable[source] = err.Error()
			continue
		}
		r, err := NewResolver(mod.Dir)
		if err != nil {
			e.unreachable[source] = err.Error()
			continue
		}
		e.resolvers[source] = r
		e.versions[source] = mod.Version
		if mod.Replaced != "" {
			e.replaced[source] = fmt.Sprintf("%s %s is replaced by %s, so every answer below "+
				"is about that tree and not about the tag named", source, mod.Version, mod.Replaced)
			e.caveats = append(e.caveats, e.replaced[source])
		}
		if source == "anthropic-sdk-go" {
			e.Pin = mod.Version
			e.spec = NewSpec(mod.Dir)
		}
	}
	for source, why := range e.unreachable {
		// A governed source nobody could open changes what a clean run means,
		// whether or not the corpus happens to cite it today: the rungs below
		// can say nothing at all about that source.
		e.caveats = append(e.caveats, fmt.Sprintf("%s could not be resolved offline, so no "+
			"rung below shape can answer for its citations: %s", source, why))
	}
	if e.Pin == "" {
		return nil, fmt.Errorf("cannot resolve %s offline, so no rung below shape can "+
			"run: %s", SDKModule, e.unreachable["anthropic-sdk-go"])
	}
	sort.Strings(e.caveats)
	return e, nil
}

// Resolution is rung 2. It judges only citations stamped at the version the
// module graph actually holds, with the polarity the citation's form asks for.
func (e *Env) Resolution(cs []Citation) []Finding {
	positive := index(cs, true)
	var out []Finding
	for _, c := range cs {
		v, known := e.versions[c.Source]
		if !known || c.Tag != v {
			continue // a tag this gate cannot open; rung 3 speaks about it instead
		}
		out = append(out, e.judge(c, positive)...)
	}
	return out
}

// judge asks the one question rung 2 exists for, and phrases the answer as the
// citation's own claim being wrong rather than as "not found".
//
// positive indexes the corpus's positive anchors, for the one `absent at` the
// pin cannot contradict and must still accept: a deleted file's disposition.
func (e *Env) judge(c Citation, positive units) []Finding {
	if c.Loc.Kind == "span" {
		found, err := e.falsifySpan(c, e.resolvers[c.Source], c.Tag)
		if err != nil {
			return []Finding{e.finding(c, "span-uncheckable", fmt.Sprintf(
				"%q could not be read at %s: %v", c.Raw, c.Tag, err))}
		}
		return found
	}
	exists, unique, err := e.exists(c)
	var gone NotShipped
	switch {
	case errors.As(err, &gone) && c.Positive():
		return []Finding{e.finding(c, "vanished-at-stamp", fmt.Sprintf(
			"%q claims this exists at %s, the version go.mod pins, and the module ships no %s "+
				"there", c.Raw, c.Tag, gone.File))}
	case errors.As(err, &gone) && positive.cover(c, c.Loc.names(), func(tag string) bool {
		return newer(c.Tag, tag)
	}):
		// The anchor beside it, checked on the same file and symbol at an
		// earlier tag, says the file was there, which leaves the one reading of
		// this the pin can confirm: the file has gone. Refused, a deleted file
		// was the one transition no line could disposition.
		return nil
	case errors.As(err, &gone):
		return []Finding{e.finding(c, "unresolvable", fmt.Sprintf(
			"%q claims a symbol is absent from %s, which the module does not ship at %s: "+
				"nothing in a missing file can resolve, so no parser could ever contradict "+
				"this — name the file the symbol was declared in", c.Raw, gone.File, c.Tag))}
	case err != nil:
		return []Finding{e.finding(c, "unresolvable", fmt.Sprintf(
			"%q could not be resolved at %s: %v", c.Raw, c.Tag, err))}
	}
	var out []Finding
	switch {
	case c.Positive() && !exists:
		out = append(out, e.finding(c, "vanished-at-stamp", fmt.Sprintf(
			"%q claims this exists at %s, the version go.mod pins, and it does not",
			c.Raw, c.Tag)))
	case !c.Positive() && exists:
		out = append(out, e.finding(c, "returned-at-stamp", fmt.Sprintf(
			"%q claims this is gone at %s, the version go.mod pins, and it resolves: a "+
				"negative claim that starts resolving again is as wrong as a positive one "+
				"that stops", c.Raw, c.Tag)))
	}
	// Ambiguity is a question about a positive anchor: which declaration it
	// means. An `absent at` anchor whose name resolves at all is already wrong,
	// whichever declaration that is.
	if c.Loc.Kind == "symbol" && c.Positive() && exists && !unique {
		where, advice := c.Loc.File, "qualify it by its receiver or struct, or fall back "+
			"to a span with `no-unique-name`"
		if !strings.HasSuffix(c.Loc.File, ".go") {
			// The count came from the packages the document links the name
			// into, because no parser reads the file the citation named — so a
			// span there is not an alternative and saying "this file declares it
			// twice" would be false.
			where = "the packages " + c.Loc.File + " links it into"
			advice = "qualify it by its receiver or struct, or cite the Go file that " +
				"declares the one you mean"
		}
		out = append(out, e.finding(c, "ambiguous-symbol", fmt.Sprintf(
			"%q names a symbol that %s declare more than once: %s", c.Raw, where, advice)))
	}
	return out
}

func (e *Env) finding(c Citation, rule, msg string) Finding {
	return Finding{File: c.File, Line: c.Line, Rule: rule, Msg: msg}
}

// exists answers the citation's own question for whichever locator it uses, at
// the pin. With more than one symbol the quantifier follows the polarity. A
// positive anchor claims every symbol it names, so found means all of them
// resolve and one that has gone is the citation being wrong. An `absent at`
// anchor claims none of them is there, so found means any one resolves — read as
// "all", a negative claim over two symbols passed while one of them was back.
// unique is whether every symbol that resolved did so exactly once.
func (e *Env) exists(c Citation) (found, unique bool, err error) {
	if c.Loc.Kind == "schema" {
		// The grammar gives a schema path to SpecSource alone, so this is the
		// spec that source bundles.
		if e.spec == nil {
			return false, false, fmt.Errorf("no spec was opened for this run")
		}
		if !e.spec.Available() {
			return false, false, fmt.Errorf("the module ships no readable spec: %v", e.spec.Err())
		}
		ok, err := e.spec.Resolve(c.Loc.Path)
		return ok, ok, err
	}
	r, ok := e.resolvers[c.Source]
	if !ok {
		// Naming the reason matters: "unreachable" and "not a module we govern"
		// send a reader to different places, and only the first is a machine
		// this run happened to be on.
		if why, bad := e.unreachable[c.Source]; bad {
			return false, false, fmt.Errorf("%s could not be resolved offline: %s", c.Source, why)
		}
		return false, false, fmt.Errorf("%s is not a module this gate can resolve", c.Source)
	}
	all, some := true, false
	unique = true
	for _, sym := range c.Loc.Symbols {
		f, u, err := r.Resolve(c.Loc.File, sym)
		if err != nil {
			return false, false, err
		}
		all, some = all && f, some || f
		if f && !u {
			unique = false
		}
	}
	if c.Positive() {
		return all, unique, nil
	}
	return some, unique, nil
}

// falsifySpan checks the reason a span gave for not being a symbol, against the
// source at the tag the span itself was stamped with. Every member of the closed
// set is a property of the source, so wherever that tag can be opened each one
// can be contradicted — which is what stops the fallback from being a way out.
//
// Nothing here returns quietly. A file the module does not ship at the span's
// own tag contradicts the span as surely as a false reason does. A file that
// could not be read is the error, a span nobody could check: its callers name
// it, because a rung that emitted nothing for it would hand back a clean bill of
// health it did not earn.
func (e *Env) falsifySpan(c Citation, r *Resolver, at string) ([]Finding, error) {
	// A range whose upper bound is past the last line has drifted, and it can
	// do so while still touching declarations — which is why this is asked
	// before anything about what the range encloses. A rung that only counted
	// the declarations still inside would report nothing at all about it.
	lines, err := r.LineCount(c.Loc.File)
	var gone NotShipped
	if errors.As(err, &gone) {
		return []Finding{e.finding(c, "span-not-shipped", fmt.Sprintf(
			"%q spans lines of %s, which the module does not ship at %s: the file has gone, "+
				"or the path never named one", c.Raw, gone.File, at))}, nil
	}
	if err != nil {
		return nil, err
	}
	if c.Loc.To > lines {
		return []Finding{e.finding(c, "span-past-eof", fmt.Sprintf(
			"%q spans lines %d-%d, but at %s the file has %d: the range runs off the end, "+
				"so whatever it was written for has moved",
			c.Raw, c.Loc.From, c.Loc.To, at, lines))}, nil
	}
	// A file no parser reads has no declarations to cross and no names to
	// repeat, so `non-go` is the one reason a span over it can give — the grammar
	// refuses the others there. It is still not true of every such file: a
	// documentation file that names the module's symbols has a structure to
	// anchor on, and the SDK's `api.md` names one on every line. Accepting
	// `non-go` there on existence alone would let exactly the citations plan 51
	// anchors on symbols keep their line numbers for ever.
	if !strings.HasSuffix(c.Loc.File, ".go") {
		names, err := r.NamesIn(c.Loc.File, c.Loc.From, c.Loc.To)
		if err != nil {
			return nil, err
		}
		if len(names) > 0 {
			return []Finding{e.finding(c, "span-reason-false", fmt.Sprintf(
				"%q says the source has no structure to name, but at %s the range names %s, "+
					"which the module declares: anchor on the symbol instead",
				c.Raw, at, strings.Join(names, " and ")))}, nil
		}
		return nil, nil
	}
	decls, err := r.DeclsIn(c.Loc.File, c.Loc.From, c.Loc.To)
	if err != nil {
		return nil, err
	}
	if len(decls) == 0 {
		return []Finding{e.finding(c, "span-encloses-nothing", fmt.Sprintf(
			"%q spans lines %d-%d, which at %s enclose no declaration at all: the range "+
				"has drifted off whatever it was written for", c.Raw, c.Loc.From, c.Loc.To, at))}, nil
	}
	switch c.Loc.Reason {
	case "crosses-declarations":
		if len(decls) <= 1 {
			return []Finding{e.finding(c, "span-reason-false", fmt.Sprintf(
				"%q says the range crosses declarations; at %s it covers %d, so name the "+
					"symbol instead", c.Raw, at, len(decls)))}, nil
		}
	case "no-unique-name":
		// The reason claims there was no name to anchor on. The grammar joins
		// several symbols with `and`, so a range covering more than one
		// declaration is still nameable — and stopping at the single-declaration
		// case let every wider span through in silence.
		var named []string
		for _, d := range decls {
			if d.NameRepeats {
				named = nil
				break
			}
			named = append(named, fmt.Sprintf("%q", d.Name))
		}
		if len(named) > 0 {
			return []Finding{e.finding(c, "span-reason-false", fmt.Sprintf(
				"%q says the enclosing declaration's name is not unique; at %s the range "+
					"encloses %s, each declared once — name them instead",
				c.Raw, at, strings.Join(named, " and ")))}, nil
		}
	}
	return nil, nil
}

// at returns the source's tree at a given tag: the pin's resolver when the tag
// is the pin, and otherwise whatever the module cache already holds. It never
// fetches, so a tag nobody unpacked is an answerable "not here" rather than a
// download inside a report.
func (e *Env) at(source, version string) (*Resolver, error) {
	if v, ok := e.versions[source]; ok && v == version {
		return e.resolvers[source], nil
	}
	path, ok := modules[source]
	if !ok {
		return nil, fmt.Errorf("%s is a tagged checkout, not a module", source)
	}
	key := source + "@" + version
	if r, ok := e.older[key]; ok {
		return r, nil
	}
	if why, ok := e.olderErr[key]; ok {
		return nil, fmt.Errorf("%s", why)
	}
	dir, err := e.cache(path, version)
	if err == nil {
		var r *Resolver
		if r, err = NewResolver(dir); err == nil {
			e.older[key] = r
			return r, nil
		}
	}
	e.olderErr[key] = err.Error()
	return nil, err
}

// Report is rung 3's output: the two transitions a bump can produce, the span
// reasons the sources contradict, the lag behind the pin, and — as loudly as
// any of them — everything it could not check.
type Report struct {
	Pins     []string // each resolved source and the version go.mod pins it at
	Read     int      // citations in the grammar that this rung considered
	Vanished []Finding
	Returned []Finding
	// Dispositioned are the anchors gone at the pin whose unit already says so.
	// Vanished and Returned are the transitions still awaiting that line.
	Dispositioned []Finding
	Contradicted  []Finding
	Lag           []string
	Uncheckable   []Unchecked
	Caveats       []string
}

// Undispositioned counts the transitions no citation has acknowledged — what
// the pull request moving a pin may not merge with.
func (r Report) Undispositioned() int { return len(r.Vanished) + len(r.Returned) }

// unitName is one name an anchor gives, keyed by where it is written.
type unitName struct {
	file   string // the document or Go file the citation is in
	unit   int
	source string
	loc    string // the file the locator names inside the source
	name   string
}

// units indexes the anchors of one polarity by each name they give, to the tags
// they were stamped at.
type units map[unitName][]string

func index(cs []Citation, positive bool) units {
	u := units{}
	for _, c := range cs {
		if c.Positive() != positive {
			continue
		}
		for _, n := range c.Loc.names() {
			k := unitName{c.File, c.Unit, c.Source, c.Loc.File, n}
			u[k] = append(u[k], c.Tag)
		}
	}
	return u
}

// cover reports whether each of names is given, in c's own unit and on the same
// file of the same source, by an anchor whose tag ok accepts.
func (u units) cover(c Citation, names []string, ok func(tag string) bool) bool {
	for _, n := range names {
		if !slices.ContainsFunc(u[unitName{c.File, c.Unit, c.Source, c.Loc.File, n}], ok) {
			return false
		}
	}
	return true
}

// vanish files an anchor the pin no longer holds. It is dispositioned when an
// `absent at` beside it names everything it lost, at a tag after its stamp and
// no later than the pin: one no later than the stamp contradicts the anchor
// rather than recording a bump since, and one ahead of the pin records a bump
// that has not happened. Otherwise the finding carries the line that would
// disposition it.
func (r *Report) vanish(c Citation, lost []string, absent units, pin, msg string) {
	f := Finding{File: c.File, Line: c.Line, Rule: "vanished-at-pin", Msg: msg}
	if absent.cover(c, lost, func(tag string) bool { return newer(tag, c.Tag) && !newer(tag, pin) }) {
		r.Dispositioned = append(r.Dispositioned, f)
		return
	}
	f.Msg += fmt.Sprintf(" — if the claim held at %s, record the bump beside it: `absent at %s %s — %s %s`",
		c.Tag, c.Source, pin, c.Loc.File, strings.Join(lost, " and "))
	r.Vanished = append(r.Vanished, f)
}

// lost names what a vanished positive anchor gave that the pin does not hold.
func (e *Env) lost(c Citation) []string {
	if c.Loc.Kind != "symbol" {
		return c.Loc.names()
	}
	var out []string
	for _, sym := range c.Loc.Symbols {
		if found, _, _ := e.resolvers[c.Source].Resolve(c.Loc.File, sym); !found {
			out = append(out, sym)
		}
	}
	return out
}

// Unchecked is one reason this run could not answer for some citations, and the
// anchors that reason covers. The reason alone is not a listing: "3 citations
// were not checked" leaves a migrator with nothing to open, and being able to
// go and look at them is the whole of why they are listed rather than skipped.
type Unchecked struct {
	Why     string
	Anchors []string
}

func (u Unchecked) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d anchor(s)", u.Why, len(u.Anchors))
	for _, a := range u.Anchors {
		fmt.Fprintf(&b, "\n      %s", a)
	}
	return b.String()
}

// NameOurs lists the coordinates into this repository that rung 1 read, under
// "not checked". Plan 51 excludes them from every rung — git holds our history,
// not a tag — and has the report name them anyway, so that the list of what
// went unchecked is a list and not a silence. They are the ones rung 1 reads:
// every one in a Go comment, and in the registry those on a line that names a
// governed source.
func (r *Report) NameOurs(anchors []string) {
	if len(anchors) == 0 {
		return
	}
	sorted := append([]string(nil), anchors...)
	sort.Strings(sorted)
	r.Uncheckable = append(r.Uncheckable, Unchecked{
		Why: "a coordinate into this repository's own source, which plan 51 checks at " +
			"no rung: git holds its history, not a tag",
		Anchors: sorted,
	})
	sort.Slice(r.Uncheckable, func(i, j int) bool {
		return r.Uncheckable[i].Why < r.Uncheckable[j].Why
	})
}

// anchorOf names a citation the way a reader can open it. A unit test's
// citation has no file, and quoting the clause is the only address it has.
func anchorOf(c Citation) string {
	if c.File == "" {
		return c.Raw
	}
	return fmt.Sprintf("%s:%d", c.File, c.Line)
}

// Bump is rung 3. It resolves every anchor it can against the pin, whatever the
// citation's stamp, and never fails.
func (e *Env) Bump(cs []Citation) Report {
	rep := Report{Read: len(cs), Caveats: e.caveats}
	for source, v := range e.versions {
		rep.Pins = append(rep.Pins, source+" "+v)
	}
	sort.Strings(rep.Pins)
	absent := index(cs, false)
	lag := map[string]int{}
	uncheckable := map[string][]string{}
	unchecked := func(c Citation, why string) {
		uncheckable[why] = append(uncheckable[why], anchorOf(c))
	}

	for _, c := range cs {
		// Lag is a property of the stamp alone, so it is counted before
		// anything else can decide the citation is unreadable. A span whose tag
		// nobody holds is still four versions behind, and a lag list that
		// omitted it would understate the distance the corpus has drifted.
		//
		// A stamp ahead of the pin is not lag, and the pin cannot judge it: a
		// symbol added after the pin reads there as vanished, and one removed
		// after it as back. Someone who read a newer checkout wrote it, and
		// reporting either transition would send a human to disposition a bump
		// that has not happened.
		if v, ok := e.versions[c.Source]; ok && c.Tag != v {
			if newer(c.Tag, v) {
				unchecked(c, fmt.Sprintf("stamped ahead of the pin %s, which predates what it "+
					"describes", v))
				continue
			}
			lag[fmt.Sprintf("%s %s (pin %s)", c.Source, c.Tag, v)]++
		}
		if _, ok := modules[c.Source]; !ok {
			unchecked(c, fmt.Sprintf("%s is a tagged checkout, not a module: rung 1 only", c.Source))
			continue
		}
		if c.Loc.Kind == "span" {
			// A span's numbers mean nothing at any tag but its own, so the pin
			// cannot judge it. Where the cache holds that tag, its reason is
			// falsified there; where it does not, that is said rather than
			// passed over.
			r, err := e.at(c.Source, c.Tag)
			if err != nil {
				unchecked(c, fmt.Sprintf("a line span stamped %s: %v", c.Tag, err))
				continue
			}
			found, err := e.falsifySpan(c, r, c.Tag)
			if err != nil {
				// The reason names the error and not the citation, so spans that
				// failed the same way are listed together under it.
				unchecked(c, fmt.Sprintf("a line span that could not be read at its own tag %s: %v",
					c.Tag, err))
				continue
			}
			rep.Contradicted = append(rep.Contradicted, found...)
			continue
		}
		pin := e.versions[c.Source]
		exists, unique, err := e.exists(c)
		var gone NotShipped
		switch {
		case errors.As(err, &gone) && c.Positive():
			// A file that went away is a transition like a symbol that did —
			// a deletion or a rename for a human to disposition.
			rep.vanish(c, c.Loc.names(), absent, pin, fmt.Sprintf(
				"%q was checked at %s, and the pin %s ships no %s", c.Raw, c.Tag, pin, gone.File))
			continue
		case errors.As(err, &gone):
			unchecked(c, "an `absent at` anchor on a file the pin does not ship, which nothing "+
				"can contradict")
			continue
		case err != nil:
			unchecked(c, err.Error())
			continue
		}
		switch {
		case c.Positive() && !exists:
			rep.vanish(c, e.lost(c), absent, pin, fmt.Sprintf(
				"%q was checked at %s and no longer resolves at the pin %s", c.Raw, c.Tag, pin))
		case !c.Positive() && exists:
			// Nothing written beside it disposes of this: plan 51's disposition
			// is dropping the clause, since the claim it made has stopped being
			// the reference's state.
			rep.Returned = append(rep.Returned, e.finding(c, "returned-at-pin", fmt.Sprintf(
				"%q was absent at %s and resolves again at the pin %s — drop the `absent at` "+
					"clause, and re-read the claim it was part of", c.Raw, c.Tag, pin)))
		case c.Positive() && c.Loc.Kind == "symbol" && !unique:
			// Resolving says only that some declaration of the name survived,
			// not that the one the citation meant did. Passing it would be the
			// same silence rung 2 reports as ambiguous-symbol.
			unchecked(c, "a symbol the pin declares more than once, so its resolving says "+
				"nothing about the declaration the citation meant")
		}
	}
	rep.Lag = tally(lag, "citation")
	// An unreachable source is a caveat on the whole run, set by NewEnv. It is
	// not repeated here: every citation of one already reached this list
	// through the resolver it could not find, carrying its own anchor.
	for why, anchors := range uncheckable {
		sort.Strings(anchors)
		rep.Uncheckable = append(rep.Uncheckable, Unchecked{Why: why, Anchors: anchors})
	}
	sort.Slice(rep.Uncheckable, func(i, j int) bool {
		return rep.Uncheckable[i].Why < rep.Uncheckable[j].Why
	})
	return rep
}

// Unresolvable names the governed sources the corpus cites whose tag this run
// cannot judge: one it could not open at all, whose citations rung 2 skips, and
// one a `replace` or a `go.work` points at another tree, whose citations rung 2
// judges against that tree while naming the tag. A caller that failed on
// findings alone would exit clean on either — having checked nothing, or having
// certified a fork.
func (e *Env) Unresolvable(cs []Citation) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if seen[c.Source] {
			continue
		}
		seen[c.Source] = true
		if why, bad := e.unreachable[c.Source]; bad {
			out = append(out, fmt.Sprintf("%s: %s", c.Source, why))
		}
		if why, bad := e.replaced[c.Source]; bad {
			out = append(out, why)
		}
	}
	sort.Strings(out)
	return out
}

// newer reports whether tag is a later release than pin. A citation's tag is
// always vMAJOR.MINOR.PATCH, since the grammar admits nothing else, but the pin
// is whatever go.mod holds and may carry a pre-release or pseudo-version suffix
// — and a release with the same numbers comes after that.
//
// The numbers are compared as digit strings, by length and then lexically, which
// orders them because neither side writes a leading zero: the grammar refuses
// one in a tag, and a module version never carries one. A tag is text a citation
// wrote, and one too large for an int is still a later release: converted, it
// read as zero and passed for lag.
func newer(tag, pin string) bool {
	t, _ := release(tag)
	p, pre := release(pin)
	for i := range t {
		if len(t[i]) != len(p[i]) {
			return len(t[i]) > len(p[i])
		}
		if t[i] != p[i] {
			return t[i] > p[i]
		}
	}
	return pre
}

// release splits a version into its three numbers, and whether it carries a
// pre-release suffix.
func release(v string) ([3]string, bool) {
	core, suffix, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	core, _, _ = strings.Cut(core, "+")
	var out [3]string
	copy(out[:], strings.SplitN(core, ".", 3))
	return out, suffix != ""
}

func tally(m map[string]int, noun string) []string {
	var out []string
	for k, n := range m {
		out = append(out, fmt.Sprintf("%s — %d %s(s)", k, n, noun))
	}
	sort.Strings(out)
	return out
}

// String renders the report the way `make sdk-bump-report` prints it. Every
// section is printed even when empty, because a section that disappears when it
// has nothing to say cannot be told from one that was never run.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sdk bump report — pins %s; %d citation(s) in the grammar\n",
		strings.Join(r.Pins, ", "), r.Read)
	if r.Read == 0 {
		// Empty sections are indistinguishable from clean ones. The corpus is
		// migrated by plan 51's slices 2 and 3; until then this rung has
		// nothing to resolve and must say so rather than look satisfied.
		b.WriteString("\nNo citation is written in this grammar yet, so every section " +
			"below is empty because\nthere was nothing to check — not because nothing " +
			"was wrong. Rung 1's findings are\nthe migration this is waiting for.\n")
	}
	for _, c := range r.Caveats {
		fmt.Fprintf(&b, "\ncaveat: %s\n", c)
	}
	section := func(title string, lines []string) {
		fmt.Fprintf(&b, "\n%s (%d)\n", title, len(lines))
		if len(lines) == 0 {
			b.WriteString("  none\n")
			return
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	strs := func(fs []Finding) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.String())
		}
		return out
	}
	// Plan 51 specifies three lists and, under them, what went unchecked. The
	// two polarities are one list because each entry asks the same thing of a
	// human — a disposition for a deletion, a rename or a reinstatement — and
	// the transitions already dispositioned follow it, so what a bump's pull
	// request must answer is not buried under what earlier bumps answered. A
	// span whose range has drifted off its declarations sits with the spans
	// whose reason is contradicted, since in both the source at the span's own
	// tag disagrees with how the span was written.
	section("transitions awaiting a disposition — anchors gone at the pin, and `absent at` "+
		"anchors resolving again there", append(strs(r.Vanished), strs(r.Returned)...))
	section("transitions already dispositioned — anchors gone at the pin, beside an `absent at` "+
		"stamped after them", strs(r.Dispositioned))
	section("line spans the sources contradict", strs(r.Contradicted))
	section("stamps behind the pin", r.Lag)
	var unchecked []string
	for _, u := range r.Uncheckable {
		unchecked = append(unchecked, u.String())
	}
	section("not checked, and why", unchecked)
	return b.String()
}
