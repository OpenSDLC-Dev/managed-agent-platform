#!/usr/bin/env python3
"""Fail if a workflow pipeline stage can exit 0 before it has read its input.

Under `pipefail` a pipeline's status is the RIGHTMOST NON-ZERO one, so a reader
that stops early takes its writer down with it: the reader exits, the pipe
closes under a writer still writing, the writer dies of SIGPIPE, and its 141
becomes the pipeline's status. The step then announces a miss for the very
thing it just matched.

`pipefail` is in effect where a block says `set -o pipefail`, and where the step
says `shell: bash` — GitHub runs that one as `bash --noprofile --norc -eo
pipefail {0}`, where the default it replaces, `bash -e {0}`, has no pipefail at
all. #763's step was of the first kind.

Measured on #763's shape, a 200 KB input through `echo "$rendered" | grep -q`:
match present at the top gives 141, match absent gives 1. SIGPIPE therefore
cannot happen unless the reader already found what it was looking for — but
WHICH WAY THAT FAILS depends on how the verdict is consumed, and both ways are
bad:

  pipeline || { echo "not found"; exit 1; }   the 141 runs the `||` branch, so
                                              the step reddens for the very
                                              condition it was checking FOR

  pipeline && { echo "found"; exit 1; }       the 141 SKIPS the `&&` branch, so
                                              the step passes although the
                                              forbidden thing was found

The second is a false GREEN, the worse direction, and it is not hypothetical:
origin/main carried three of it (ci.yml:186, :399, :1371), latent only because
none of those blocks sets pipefail today — which is exactly the "armed later"
case this check exists for.

It bit a `helm template` render of about 22 KB only because macOS starts its
pipe buffer at 16 KB while Linux's is 65,536, so a bigger render could have
grown into it on a runner with nobody the wiser. The fix is never the buffer:
it is to stop the early exit. `grep PATTERN > /dev/null` reads to EOF and keeps
grep's 0/1 verdict; so does a here-string, `grep -q PATTERN <<<"$var"`, where
there is no writer to signal at all.

WHAT COUNTS AS STOPPING EARLY is "can exit 0 before EOF", not "exits early".
A reader's own non-zero status outranks the writer's 141 under pipefail, so
`awk '... { exit 1 }'` is sound where `awk '... { exit }'` is not, and an `exit`
inside an `END` block is sound because `END` runs only once the input is spent.
Measured under `bash -o pipefail` on a 5 MB writer, and not reasoned: mawk,
gawk and one-true-awk agree on all three; `grep -q`/`--quiet`/`--silent`/`-m`,
`head` and `sed`'s `q` take the writer down on GNU and BSD alike; `grep -l`
does so on BSD grep but not on GNU 3.11; and `grep -L` does so on NEITHER, so
it is not named here however plainly it reads as an early exit.

It is a SHAPE check, and it does not ask whether the block it found the shape
in sets pipefail. Both are deliberate: it cannot know how big any particular
render is, "small enough today" is the assumption that rotted here, and a
`set -euo pipefail` added to a block years later must not silently arm a
pipeline written under the old rule. For the same reason a pipeline whose
status is thrown away with `|| true` is still refused — that is one edit away
from mattering, and the fix costs nothing.

READING NOTHING MUST NEVER READ AS CLEAN. That is the failure this file exists
to prevent, and the one it is most able to commit, since it walks shell by
hand. So it REFUSES, loudly and by name, every shape it cannot model rather
than skipping it: a heredoc whose delimiter never arrives, a `<<` that opens no
delimiter it recognises, a folded `run: >` scalar (whose newlines fold and
whose `#` does not start a comment), and a `run:` written as a multi-line plain
scalar. It cross-checks its own glob against `git ls-files`, as pins_test.py
does, so a narrowed glob cannot quietly drop a workflow. And it reads `run:`
bodies as shell and the rest of the file not at all, because the rest is not
shell — an apostrophe in a step name would open a quoted run that swallows
everything after it.

WHAT IT STILL CANNOT SEE: a reader reached through a shell function, an alias,
or a script the workflow calls; a wrapper whose own option takes a separate word
(`sudo -u ci grep -q x`), which hides the command behind it; and a loop that
reads and then breaks (`while read …; do break; done`, measured at 141 where the
same loop without the break gives 0), since what stops it is control flow rather
than a command this can name. It reads the workflow files, not the programs they
run.

Run: make pipes-test
"""
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"

# `run:` as a mapping key, with or without the `- ` that starts its step.
RUN_KEY = re.compile(r"^(\s*(?:-\s+)?)run:(.*)$")

# `<<WORD`, `<<-WORD`, `<<'WORD'` open a heredoc whose body is data, not code.
# `<<<`, a here-string, is consumed before this is ever tried.
HEREDOC = re.compile(r"<<(-?)[ \t]*(?:'([^'\n]+)'|\"([^\"\n]+)\"|([A-Za-z_][\w-]*))")

# Words that precede the real command without changing what it reads.
WRAPPERS = frozenset({"command", "env", "builtin", "exec", "nohup", "stdbuf", "time", "sudo"})
ASSIGN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
DURATION = re.compile(r"^[0-9]+(?:\.[0-9]+)?[smhd]?$")   # `timeout`'s own argument
GREP = frozenset({"grep", "egrep", "fgrep"})
GREP_TAKES_ARG = frozenset({"-e", "--regexp", "-f", "--file", "-m", "--max-count"})

# sed's `q`/`Q` as a COMMAND: at the start of the script or after `;`/`{`/`}`/a
# newline, behind an optional address. `s/x/q/p` does not match, and must not.
SED_QUIT = re.compile(r"""(?:^|[;{}\n])\s*(?:/(?:\\.|[^\\/])*/|\\(.)(?:\\.|.)*?\1|[0-9,$~+]*)\s*!?\s*[qQ]\b""")
SED_TAKES_ARG = frozenset({"-e", "--expression", "-f", "--file", "-i", "--in-place", "-l", "--line-length"})
# awk's `exit`, with the status it exits with. No status, or 0, is the hazard.
AWK_EXIT = re.compile(r"\bexit\b\s*\(?\s*([0-9]*)")
AWK_TAKES_ARG = frozenset({"-v", "-f", "-F"})
# `END { … }` runs after the input is spent, so an exit inside one is not early.
AWK_END = re.compile(r"\bEND\b\s*\{")


class Unreadable(Exception):
    """A shape this scan cannot model. Refused by name, never skipped."""


def _dedent(body):
    """Strip a YAML block scalar's own indentation, as YAML does before the
    shell ever sees it. Without this a heredoc terminator carries the block's
    indent and could not be compared with what bash compares."""
    first = next((ln for ln in body if ln.strip()), "")
    pad = first[: len(first) - len(first.lstrip())]
    return [ln[len(pad):] if ln.startswith(pad) else ln.lstrip() for ln in body]


def run_blocks(lines):
    """`(first_body_lineno, body_text)` for every `run:` value in a workflow file.

    A block scalar's body is the following lines indented past the `run:` key,
    dedented. An inline scalar is the whole command. A bare `run:` is the
    `defaults:` mapping, not a step, and holds no shell at all.
    """
    out = []
    i = 0
    while i < len(lines):
        m = RUN_KEY.match(lines[i])
        if not m:
            i += 1
            continue
        col = len(m.group(1))          # the column `run:` itself starts at
        rest = m.group(2).strip()
        if not rest:                   # `defaults:` → `run:` → `working-directory:`
            i += 1
            continue
        if rest[0] == ">":
            raise Unreadable(
                "%d: a folded `run: >` scalar — its newlines fold into spaces and its"
                " `#` starts no comment, so reading it as written would be reading"
                " something the shell never sees" % (i + 1)
            )
        if rest[0] != "|":
            nxt = lines[i + 1] if i + 1 < len(lines) else ""
            if nxt.strip() and (len(nxt) - len(nxt.lstrip())) > col:
                raise Unreadable(
                    "%d: a `run:` written as a multi-line PLAIN scalar. Only a block"
                    " scalar and a one-line value are tracked as bodies, which every"
                    " `run:` in this repository is." % (i + 1)
                )
            if len(rest) > 1 and rest[0] == rest[-1] and rest[0] in "'\"":
                rest = rest[1:-1]      # a YAML-quoted scalar; the quotes are YAML's
            out.append((i + 1, rest))
            i += 1
            continue
        j = i + 1
        body = []
        while j < len(lines):
            ln = lines[j]
            if ln.strip() and (len(ln) - len(ln.lstrip())) <= col:
                break
            body.append(ln)
            j += 1
        out.append((i + 2, "\n".join(_dedent(body))))
        i = j
    return out


def stages(text, first_line=1):
    """Every pipeline stage in `text` that has a writer upstream of it.

    Returns `[(lineno, words)]`. The shell grammar this walks is the part that
    decides where a pipeline begins and ends: quoting, comments, `$( )`,
    arithmetic, `[[ ]]`, heredocs and here-strings, `case` arms, line
    continuation, and the difference between `|` and `||`.
    """
    found = []
    words, cur, has_writer, dq = [], "", False, False
    lineno = stage_line = first_line
    subst = []          # saved state across `$( )`
    heredocs = []       # delimiters opened but not yet consumed
    depth = 0           # open `(` in code — what tells a subshell from a case arm
    case_level = 0      # open `case` … `esac`
    case_pending = False  # saw `case`, waiting for its `in`
    in_pattern = False  # inside a case arm's pattern list, where `|` alternates
    i, n = 0, len(text)

    def flush():
        nonlocal cur, case_level, case_pending, in_pattern
        if not cur:
            return
        w, cur = cur, ""
        words.append(w)
        if w == "case":
            case_level += 1
            case_pending = True
        elif case_pending and w == "in":
            case_pending = False
            in_pattern = True
        elif w == "esac" and case_level:
            case_level -= 1
            in_pattern = False

    def end_stage(keep=True):
        nonlocal words, stage_line
        flush()
        if words and has_writer and keep:
            found.append((stage_line, words))
        words = []
        stage_line = lineno

    def end_pipeline(keep=True):
        nonlocal has_writer
        end_stage(keep)
        has_writer = False

    def span(opener, closer, why):
        """Consume a balanced `opener … closer` as one word."""
        nonlocal cur, i, lineno
        j = text.find(closer, i + len(opener))
        if j < 0:
            raise Unreadable("%d: %s is never closed" % (lineno, why))
        chunk = text[i : j + len(closer)]
        cur += chunk
        lineno += chunk.count("\n")
        i = j + len(closer)

    while i < n:
        c = text[i]

        if c == "\\" and i + 1 < n:
            if text[i + 1] == "\n":          # a continued line is one line of code
                lineno += 1
                i += 2
                continue
            cur += text[i + 1]
            i += 2
            continue

        if c == "'" and not dq:
            j = text.find("'", i + 1)
            if j < 0:
                raise Unreadable("%d: a single-quoted string is never closed" % lineno)
            body = text[i + 1 : j]
            cur += body
            lineno += body.count("\n")
            i = j + 1
            continue

        if c == '"':
            dq = not dq
            i += 1
            continue

        if c == "$" and text[i : i + 3] == "${{":     # a GitHub expression
            span("${{", "}}", "a `${{ … }}` expression")
            continue

        if c == "$" and text[i : i + 3] == "$((":     # arithmetic, where `|` and `<<` are operators
            span("$((", "))", "an arithmetic `$(( … ))`")
            continue

        if c == "$" and text[i : i + 2] == "$(":
            subst.append((words, cur, has_writer, dq, stage_line, depth))
            words, cur, has_writer, dq, depth = [], "", False, False, 0
            stage_line = lineno
            i += 2
            continue

        if c == "$" and text[i : i + 2] == "${":      # a parameter, not a group
            span("${", "}", "a `${ … }` parameter")
            continue

        if dq:
            if c == "\n":
                lineno += 1
            cur += c
            i += 1
            continue

        if c == "#" and not cur:        # a comment runs to the end of the line
            j = text.find("\n", i)
            i = n if j < 0 else j
            continue

        if text[i : i + 2] == "((" and not cur:       # arithmetic as a command
            span("((", "))", "an arithmetic `(( … ))`")
            continue

        if text[i : i + 2] == "[[" and not cur:       # a test, where `|` alternates
            span("[[", "]]", "a `[[ … ]]` test")
            continue

        if c == ")":
            # A pending `esac` decides whether this closes an arm or a group, so
            # it has to be a word before the question is asked — otherwise
            # `$(… esac)` takes the arm branch and never pops its substitution,
            # and the double quote the substitution sat in is lost with it.
            flush()
            if in_pattern:              # closes a case arm's pattern list
                in_pattern = False
                end_pipeline(keep=False)
                i += 1
                continue
            end_stage()
            if depth:
                depth -= 1
            elif subst:
                words, cur, has_writer, dq, stage_line, depth = subst.pop()
                cur += "_"
            else:
                has_writer = False
            i += 1
            continue

        if c == "(":
            if in_pattern:              # `case x in (a|b)` — arm syntax, not a subshell
                i += 1
                continue
            # A group straight after a pipe reads that pipe: `... | (grep -q x)`.
            if not (has_writer and not words and not cur):
                end_pipeline()
            depth += 1
            i += 1
            continue

        if c == "{":
            if not (has_writer and not words and not cur):
                end_pipeline()
            i += 1
            continue

        if c == "}":
            end_pipeline()
            i += 1
            continue

        if c == "|":
            if in_pattern:              # alternation between case patterns
                cur += c
                i += 1
            elif text[i : i + 2] == "||":
                end_pipeline()
                i += 2
            else:
                end_stage()
                has_writer = True
                i += 2 if text[i : i + 2] == "|&" else 1
            continue

        if c == "&":
            # `2>&1` ends the stage a word early. That costs nothing: the
            # command word, which is all `early_reader` reads, is already in.
            end_pipeline()
            i += 2 if text[i : i + 2] == "&&" else 1
            continue

        if c == ";":
            end_pipeline()
            if text[i : i + 2] == ";;":
                if case_level:
                    in_pattern = True   # the next arm's pattern list begins
                i += 2
            else:
                i += 1
            continue

        if c == "\n":
            lineno += 1
            i += 1
            if heredocs:
                for delim, strip_tabs in heredocs:
                    closed = False
                    while i < n:
                        j = text.find("\n", i)
                        j = n if j < 0 else j
                        line = text[i:j]
                        i = j + 1
                        lineno += 1
                        if (line.lstrip("\t") if strip_tabs else line) == delim:
                            closed = True
                            break
                    if not closed:
                        raise Unreadable(
                            "%d: a heredoc opened with `%s` is never closed, so every"
                            " line after it would go unread" % (lineno, delim)
                        )
                heredocs = []
                end_pipeline()
                continue
            # A line that ended ON a pipe continues into the next one.
            if not (has_writer and not words and not cur):
                end_pipeline()
            continue

        if c == "<" and text[i : i + 2] == "<<":
            if text[i : i + 3] == "<<<":        # a here-string has no body at all
                cur += "<<<"
                i += 3
                continue
            m = HEREDOC.match(text, i)
            if not m:
                raise Unreadable(
                    "%d: a `<<` that opens no delimiter this scan recognises" % lineno
                )
            heredocs.append((m.group(2) or m.group(3) or m.group(4), m.group(1) == "-"))
            i = m.end()
            continue

        if c.isspace():
            flush()
            i += 1
            continue

        cur += c
        i += 1

    end_pipeline()
    # Ending mid-quote or mid-substitution means the walk lost its place, and a
    # walk that lost its place must not report a clean verdict.
    if dq:
        raise Unreadable("%d: a double-quoted string is never closed" % lineno)
    if subst:
        raise Unreadable("%d: a `$( )` substitution is never closed" % lineno)
    return found


def early_reader(words):
    """Why this stage can exit 0 before its input ends, or None."""
    w = list(words)
    while w:
        lead = w[0].rsplit("/", 1)[-1]
        if ASSIGN.match(w[0]) or lead in WRAPPERS:
            w.pop(0)
            while w and w[0].startswith("-") and w[0] != "--":
                w.pop(0)               # the wrapper's own flags, not the reader's
            continue
        if lead == "timeout":          # the one wrapper carrying an argument of its own
            w.pop(0)
            while w and (w[0].startswith("-") or DURATION.match(w[0])):
                w.pop(0)
            continue
        break
    if not w:
        return None
    name = w[0].rsplit("/", 1)[-1]
    rest = w[1:]

    if name == "head":
        return "head stops after N lines"

    if name == "read":
        return "read stops at the first delimiter"

    if name == "dd" and any(a.startswith("count=") for a in rest):
        return "dd count=N stops after N blocks"   # a bare `dd` reads to EOF

    if name in ("mapfile", "readarray") and any(
            a.startswith("-") and "n" in a[1:] for a in rest):   # `-tn` clusters too
        return "mapfile -n stops after N lines"    # a bare `mapfile` reads to EOF

    if name in GREP:
        k = 0
        while k < len(rest):
            a = rest[k]
            if a == "--":
                break
            if a.split("=", 1)[0] in GREP_TAKES_ARG:
                k += 1 if "=" in a else 2      # its argument is a pattern, not a flag
                if a.split("=", 1)[0] in ("-m", "--max-count"):
                    return "grep --max-count stops after N matches"
                continue
            if a.startswith("--"):
                flag = a.split("=", 1)[0]
                if flag in ("--quiet", "--silent"):
                    return "grep --quiet exits on the first match"
                if flag == "--files-with-matches":
                    return "grep -l stops at the first match on BSD grep"
            elif a.startswith("-") and len(a) > 1:
                if "q" in a[1:]:
                    return "grep -q exits on the first match"
                if "l" in a[1:]:
                    return "grep -l stops at the first match on BSD grep"
                if "m" in a[1:]:
                    return "grep -m stops after N matches"
            k += 1
        return None

    if name == "sed":
        k, seen_script = 0, False
        while k < len(rest):
            a = rest[k]
            flag = a.split("=", 1)[0]
            if flag in ("-e", "--expression"):
                arg = a.split("=", 1)[1] if "=" in a else (rest[k + 1] if k + 1 < len(rest) else "")
                if SED_QUIT.search(arg):
                    return "sed's q command quits before the input ends"
                seen_script = True
                k += 1 if "=" in a else 2
                continue
            if flag in SED_TAKES_ARG:
                seen_script = seen_script or flag in ("-f", "--file")
                k += 1 if "=" in a else 2
                continue
            if a.startswith("-") and len(a) > 1:
                k += 1
                continue
            if not seen_script and SED_QUIT.search(a):
                return "sed's q command quits before the input ends"
            break
        return None

    if name == "awk":
        k = 0
        while k < len(rest):
            a = rest[k]
            if a in AWK_TAKES_ARG:
                k += 2
                continue
            if a.startswith("-") and len(a) > 1:
                k += 1
                continue
            for m in AWK_EXIT.finditer(_without_end_blocks(a)):
                # A status is read as a NUMBER, and modulo 256 as the shell
                # reads it: `exit 00` and `exit 256` both arrive as 0, and both
                # measure 141 under pipefail, where `exit 01` measures 1.
                if m.group(1) == "" or int(m.group(1)) % 256 == 0:
                    return "awk's exit leaves the rest of the input unread"
            break
        return None

    return None


def _without_end_blocks(program):
    """The awk program with its `END { … }` blocks blanked out. They run only
    once the input is spent, so an `exit` inside one reads nothing short."""
    out, i = [], 0
    while i < len(program):
        m = AWK_END.search(program, i)
        if not m:
            out.append(program[i:])
            break
        out.append(program[i : m.start()])
        j, level = m.end(), 1
        while j < len(program) and level:
            level += (program[j] == "{") - (program[j] == "}")
            j += 1
        i = j
    return "".join(out)


def workflows():
    """The workflow files, cross-checked against git so a narrowed glob cannot
    quietly drop one — the same rung, and the same reasoning, as pins_test.py's."""
    globbed = sorted(set(WORKFLOWS.glob("*.yml")) | set(WORKFLOWS.glob("*.yaml")))
    listed = subprocess.run(
        ["git", "ls-files", "-z", "--", "*.yml", "*.yaml"],
        cwd=WORKFLOWS, capture_output=True, text=True, check=True,
    ).stdout
    # Top level only: Actions reads no workflow from a subdirectory of this one.
    tracked = set(p for p in listed.split("\0") if p and "/" not in p)
    missed = sorted(tracked - set(p.name for p in globbed))
    if missed:
        raise SystemExit(
            "git tracks workflows this scan's glob does not match, so the glob has"
            f" been narrowed and they would go unchecked: {missed}"
        )
    return globbed


def scan(path):
    """`(findings, block_count)` for one workflow file."""
    lines = path.read_text(encoding="utf-8").split("\n")
    blocks = run_blocks(lines)
    out = []
    for first, body in blocks:
        for lineno, words in stages(body, first):
            why = early_reader(words)
            if why:
                src = lines[lineno - 1].strip() if lineno - 1 < len(lines) else ""
                out.append((lineno, src, why))
    return sorted(out), len(blocks)


# (source, should-be-flagged). It self-tests before it scans, because a scanner
# that matches nothing and a repository with nothing to match look identical
# from the outside — which is the whole failure this file exists to prevent.
SELFTEST = [
    # The two lines #763 names, verbatim in shape.
    ("""            echo "$rendered" | grep -qE "^[[:space:]]*(- )?key: ${k}\\$" \\""", True),
    ("""          echo "$rendered" | grep -qE '^[[:space:]]*name: map-platform$' \\""", True),
    # A command, not a variable, on the writing side — the bigger hazard, since
    # a full `helm template` render is what gets cut off.
    ("""          helm template t "$CHART" | grep -q '^kind: RuntimeClass'""", True),
    # Every flag spelling that makes grep stop reading.
    ("""          echo "$out" | grep -qF -- '--run-connection-test'""", True),
    ("""          echo "$wrote" | grep -qxF "$k" """, True),
    ("""          cat f | grep -F -q 'x'""", True),          # q in a later flag word
    ("""          cat f | grep --quiet 'x'""", True),
    ("""          cat f | grep --silent 'x'""", True),
    ("""          cat f | grep -m 1 'x'""", True),
    ("""          cat f | grep -m1 'x'""", True),            # attached count
    ("""          cat f | grep --max-count=1 'x'""", True),
    ("""          cat f | grep -l 'x' """, True),             # BSD grep stops here
    # A second stage that stops early is as fatal as the first.
    ("""          echo "$out" | grep -A5 'name: X' | grep -q 'key: y'""", True),
    # head, however it is spelled.
    ("""          cat f | head -5""", True),
    ("""          cat f | read -r line""", True),
    ("""          cat f | dd count=1""", True),
    ("""          cat f | mapfile -n 1 arr""", True),
    ("""          cat f | readarray -n 1 arr""", True),
    ("""          cat f | mapfile -tn 1 arr""", True),         # the clustered spelling
    ("""          cat f | /usr/bin/head -1""", True),
    ("""          cat f | command head -1""", True),
    ("""          cat f | env -i head -1""", True),          # a wrapper with its own flag
    ("""          cat f | stdbuf -o0 head -1""", True),
    ("""          cat f | timeout 5 grep -q x""", True),
    ("""          cat f | timeout --foreground 5s head -1""", True),
    # The other two readers that can exit 0 before EOF.
    ("""          cat f | sed -n '1p;q'""", True),
    ("""          cat f | sed '/foo/q'""", True),            # a regex address, not a line
    ("""          cat f | sed -e '/foo/q'""", True),
    ("""          cat f | awk 'NR==1 { print; exit }'""", True),
    ("""          cat f | awk '{ exit(0) }'""", True),       # a parenthesised status
    ("""          cat f | awk 'NR==1 { exit 00 }'""", True),   # 00 is 0, measured at 141
    ("""          cat f | awk 'NR==1 { exit 256 }'""", True),  # and so is 256, modulo 256
    # Shapes the reading end hides behind.
    ('''          out="$(cat f | grep -q x)"''', True),       # inside a substitution
    ("""          cat f |& grep -q x""", True),               # bash's |&
    ("""          cat f | (grep -q x)""", True),              # a subshell reads the pipe
    ("""          echo "$rendered" |\n            grep -q 'x'""", True),      # split after `|`
    ("""          echo "$rendered" | \\\n            grep -q 'x'""", True),   # and with a backslash
    ("""          echo "$out" | grep -q x || true""", True),  # status discarded, shape kept
    ("""          curl http://x/#frag | grep -q y""", True),  # `#` mid-word is not a comment
    ('''          echo "a # b" | grep -q x''', True),          # nor is one inside quotes
    # KEEPING ITS PLACE. Each of these puts a construct that is NOT a pipeline
    # ahead of one that is, so a walker that loses its place fails the row
    # instead of passing it for the wrong reason — which is how an earlier
    # draft's here-string row passed while swallowing the line beneath it.
    ("""          grep -q x <<<"literal"\n          cat f | head -1""", True),
    ("""          grep -q x <<<word\n          cat f | head -1""", True),
    ("""          n=$(( 1 << k ))\n          cat f | head -1""", True),
    ("""          (( v = 1 | 2 ))\n          cat f | head -1""", True),
    ("""          [[ $x =~ a|head ]]\n          cat f | head -1""", True),
    ("""          cat <<'EOF'\nproducer | grep -q literal\nEOF\n          cat f | head -1""", True),
    ("""          case "$x" in a|b) echo 1 ;; esac\n          cat f | head -1""", True),
    ("""          v=$(case $x in a|b) echo 1 ;; esac)\n          cat f | head -1""", True),
    ("""          echo "${{ secrets.A }}"\n          cat f | head -1""", True),
    # And what must NOT be flagged.
    ("""          grep -q 'x' somefile""", False),             # no pipe, no writer
    ("""          grep -q 'x' <<<"$rendered" """, False),      # here-string, no writer
    ("""          echo "$out" | grep 'x' > /dev/null""", False),      # the fixed shape
    ("""          echo "$out" | grep -E 'a|b' > /dev/null""", False),  # `|` inside quotes
    ("""          echo "$out" | grep -E 'foo|grep -q bar' > /dev/null""", False),
    ("""          cat f | grep -e -quiet f""", False),         # a pattern, not a flag
    ("""          cat f | grep --line-number 'x' > /dev/null""", False),  # a long flag, not -l
    ("""          echo "$out" | awk '/x/ { print }'""", False),
    ("""          echo "$out" | awk '$1 == "path:" { exit 1 }'""", False),  # its 1 outranks 141
    ("""          cat f | awk 'END { exit !found }'""", False),  # END runs after EOF
    ("""          cat f | awk '{ exit(1) }'""", False),        # a parenthesised non-zero status
    ("""          cat f | awk 'NR==1 { exit 01 }'""", False),  # 01 is 1, measured at 1
    ("""          cat f | sed -n 's/a/q/p'""", False),         # q inside a substitution
    ("""          cat f | sed -f script.sed q.txt""", False),  # a script file and an operand
    ("""          cat f | sed 's/x/y/' q.txt""", False),       # an operand, not a script
    ("""          cat f | tail -1""", False),                  # tail reads to EOF
    ("""          cat f | dd""", False),                       # measured: a bare dd reads on
    ("""          cat f | mapfile arr""", False),              # and so does a bare mapfile
    ("""          cat f | mapfile -t arr""", False),           # -t alone reads on
    ("""          cat f | while read -r l; do :; done""", False),   # the loop reads to EOF
    ("""          cat f | grep -L 'x' > /dev/null""", False),   # measured: reads to EOF on both
    ("""          cat f | grep --files-without-match 'x' > /dev/null""", False),
    ("""          test -f x || grep -q needle x""", False),     # `||` is not a pipe
    ("""          producer | head-count""", False),             # a different command
    ("""          (( v = 1 | head ))""", False),               # arithmetic, where `|` is bitwise
    ("""          [[ $x =~ a|head ]]""", False),               # a test, where `|` alternates
    ("""          docker compose config --quiet""", False),     # not grep, not a pipe
    ('''          printf '%s\\n' "literal\n| grep -q not-a-command"''', False),  # quoted data
    ("""          echo "$out" | grep 'x' > /dev/null 2>&1""", False),  # `>&` is a redirect
    # A `case` arm's `|` alternates and its `)` closes no subshell — at any depth.
    ("""          case "$x" in\n            a) echo one ;;\n            b|head) echo two ;;\n          esac""", False),
    ("""          ( case $x in a|head) echo 1 ;; esac )""", False),
    ("""          v=$(case $x in (a|head) echo 1 ;; esac)""", False),
    # That `)` must POP the substitution, or the double quote it sat inside is
    # lost with it and the data in the quotes reads as a pipeline.
    ('''          echo "$(case $x in a) echo 1 ;; esac) | head -1"''', False),
    # A comment is prose, and prose has apostrophes. Reading one as shell opens
    # a quoted run that swallows every line after it — which is how this check
    # went blind over 200 lines of ci.yml before comments were skipped and the
    # scan was confined to `run:` bodies. One apostrophe, so it never closes.
    ("""          # the chart's render is not read here\n          cat f | head -1""", True),
]

# Shapes this scan refuses by name rather than reading wrong. Each must raise.
REFUSALS = [
    ("an unterminated heredoc", "cat <<EOF\ndata\nproducer | head -1\n", "never closed"),
    ("a heredoc closed only by an indented word", "cat <<EOF\n  EOF\nproducer | head -1\n", "never closed"),
    ("a `<<` opening no delimiter", "cat << $x\n", "opens no delimiter"),
    ("an unclosed single quote", "echo 'open\n", "single-quoted string is never closed"),
    ("an unclosed double quote", 'echo "open\ncat f | head -1\n', "double-quoted string is never closed"),
    ("an unclosed substitution", "v=$(cat f\ncat f | head -1\n", "substitution is never closed"),
]

# (workflow text, expected `(first_body_lineno, body)` per block). The scanner
# can only be right about text it is handed, so the handing-over is checked too
# — in full, because an extractor that kept only first lines would pass a test
# that compared only first lines.
BLOCKTEST = (
    """jobs:
  a:
    defaults:
      run:
        working-directory: deploy/compose
    steps:
      - name: inline
        run: echo one | head -1
      - name: quoted
        run: 'echo two | head -1'
      - run: |
          echo three
          echo four
        shell: bash
      - name: heredoc
        run: |
          cat <<EOF
          data
          EOF
""",
    [
        (8, "echo one | head -1"),
        (10, "echo two | head -1"),
        (12, "echo three\necho four"),
        (17, "cat <<EOF\ndata\nEOF\n"),   # the file's last line is blank, and kept
    ],
)

# (source, the line each finding must land on). A boolean "something was
# flagged" cannot tell a walker that kept its place from one that lost it and
# found the same thing elsewhere, so the evidence a finding prints is asserted
# too — every construct here is one whose newlines a walker could fail to count.
LINETEST = [
    ('echo "${{ secrets.A\n  }}"\ncat f | head -1', [3]),
    ('echo "${x:-a\nb}"\ncat f | head -1', [3]),
    ("n=$(( 1 +\n2 ))\ncat f | head -1", [3]),
    ("[[ $x =~ a|\nb ]]\ncat f | head -1", [3]),
    ("cat <<'EOF'\nignored\nEOF\ncat f | head -1", [4]),
    ("echo 'one\ntwo'\ncat f | head -1", [3]),
    ('echo "one\ntwo"\ncat f | head -1', [3]),
    ("echo a \\\n  b\ncat f | head -1", [3]),
]

BLOCK_REFUSALS = [
    ("a folded scalar", "    steps:\n      - run: >\n          echo one\n", "folded"),
    ("a multi-line plain scalar", "    steps:\n      - run: echo one |\n          head -1\n", "PLAIN"),
]


def selftest():
    bad = []
    for text, want in SELFTEST:
        try:
            got = any(early_reader(w) for _, w in stages(text))
        except Unreadable as e:
            bad.append("  %r\n    refused (%s), want flagged=%s" % (text.strip(), e, want))
            continue
        if got != want:
            bad.append("  %r\n    flagged=%s, want %s" % (text.strip(), got, want))
    for text, want in LINETEST:
        try:
            got = sorted(l for l, w in stages(text) if early_reader(w))
        except Unreadable as e:
            got = "refused (%s)" % e
        if got != want:
            bad.append("  %r\n    flagged on lines %s, want %s" % (text, got, want))
    for name, text, because in REFUSALS:
        try:
            stages(text)
            bad.append("  %s was read rather than refused" % name)
        except Unreadable as e:
            if because not in str(e):
                bad.append("  %s was refused for the wrong reason: %s" % (name, e))
    text, want = BLOCKTEST
    got = run_blocks(text.split("\n"))
    if got != want:
        bad.append("  run: block extraction\n    got %r\n    want %r" % (got, want))
    for name, text, because in BLOCK_REFUSALS:
        try:
            run_blocks(text.split("\n"))
            bad.append("  %s was read rather than refused" % name)
        except Unreadable as e:
            if because not in str(e):
                bad.append("  %s was refused for the wrong reason: %s" % (name, e))
    if bad:
        print("pipes_test.py's own scanner is wrong:", file=sys.stderr)
        print("\n".join(bad), file=sys.stderr)
        return False
    return True


def main():
    if not selftest():
        return 1
    if not WORKFLOWS.is_dir():
        print("no .github/workflows directory at %s" % WORKFLOWS, file=sys.stderr)
        return 1
    files = workflows()
    if not files:
        print("no workflow files found — this check would pass over an empty tree", file=sys.stderr)
        return 1
    findings, blocks = [], 0
    for f in files:
        try:
            hits, count = scan(f)
        except Unreadable as e:
            print("%s holds a shape this scan will not guess at, so it refuses to" % f.relative_to(ROOT), file=sys.stderr)
            print("report a verdict over it — %s" % e, file=sys.stderr)
            return 1
        blocks += count
        for n, src, why in hits:
            findings.append("%s:%d: %s\n    %s" % (f.relative_to(ROOT), n, why, src))
    if not blocks:
        print("no `run:` blocks found — this check would pass over unread files", file=sys.stderr)
        return 1
    if findings:
        print("a workflow pipeline stage can exit 0 before its input ends, so `pipefail`", file=sys.stderr)
        print("will report the writer's SIGPIPE (141) instead of this stage's verdict:", file=sys.stderr)
        print("", file=sys.stderr)
        for f in findings:
            print("  " + f, file=sys.stderr)
        print("", file=sys.stderr)
        print("Use `grep PATTERN > /dev/null`, which reads to EOF and keeps the same", file=sys.stderr)
        print("verdict, or `grep -q PATTERN <<<\"$var\"`, which has no writer at all.", file=sys.stderr)
        return 1
    print("ok — %d workflow files, %d run: blocks, no stage stops reading early" % (len(files), blocks))
    return 0


if __name__ == "__main__":
    sys.exit(main())
