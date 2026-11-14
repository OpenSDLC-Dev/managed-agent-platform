#!/usr/bin/env python3
"""Hold the native Console job's source and credential boundary without fixtures.

Reuse the existing guards' block-style workflow readers. These checks inspect
CI's actual inputs and shell arguments; they do not execute installs or the
native runner. The source pin also binds the runner's Docker preflight.
"""
import pathlib
import re
import runpy
import shlex
import unittest

SCRIPTS = pathlib.Path(__file__).resolve().parent
PINS = runpy.run_path(str(SCRIPTS / "pins_test.py"))
PIPES = runpy.run_path(str(SCRIPTS / "pipes_test.py"))
CONSOLE_SHA = "ba0673707c5c76b221124e1b805e0625ac589b75"


class NativeConsoleJob(unittest.TestCase):
    def setUp(self):
        lines = (SCRIPTS.parent / "workflows" / "ci.yml").read_text().splitlines()
        starts = [i for i, line in enumerate(lines) if line == "  native-console:"]
        self.assertEqual(len(starts), 1, "CI must contain one native-console job")
        start = starts[0]
        end = next((i for i in range(start + 1, len(lines))
                    if re.match(r"^  \S", lines[i])), len(lines))
        self.lines = lines[start:end]
        self.body = PINS["in_block_scalar"](self.lines)
        self.refs = PINS["references"]("ci.yml", self.lines, self.body)

    def inputs(self, ref):
        start, end = PINS["step_extent"]("ci.yml", self.lines, self.body, ref)
        block = PINS["with_block"]("ci.yml", self.lines, self.body, ref, start, end)
        self.assertIsNotNone(block, "action must declare its inputs")
        inputs = {}
        for i in PINS["input_lines"](self.lines, self.body, block, end, ref.column):
            code = PINS["split_comment"](self.lines[i])[0]
            match = PINS["NAMED_INPUT"].match(code)
            self.assertIsNotNone(match, "action input must be readable")
            key = match.group("key")
            self.assertNotIn(key, inputs, "duplicate action input")
            inputs[key] = match.group("value").strip("\"'")
        return inputs

    def test_unconditional_hosted_job(self):
        headers = {}
        for i, line in enumerate(self.lines):
            if i in self.body:
                continue
            code = PINS["split_comment"](line)[0]
            self.assertIsNone(re.match(
                r"^\s*(?:-\s+)?(?:if|needs|continue-on-error|env|defaults):", code),
                "native coverage must not depend on secrets or suppress failures")
            if re.match(r"^    \S", code):
                match = PINS["NAMED_INPUT"].match(code)
                self.assertIsNotNone(match)
                key = match.group("key")
                self.assertNotIn(key, headers)
                headers[key] = match.group("value")
        self.assertEqual(headers, {
            "name": "native-console", "runs-on": "ubuntu-latest",
            "timeout-minutes": "30", "steps": "",
        })

    def test_source_checkouts_drop_credentials(self):
        checkouts = [ref for ref in self.refs if PINS["is_checkout"](ref)]
        self.assertEqual(len(checkouts), 2, "platform and Console need separate checkouts")
        self.assertEqual(self.inputs(checkouts[0]), {"persist-credentials": "false"})
        self.assertEqual(self.inputs(checkouts[1]), {
            "repository": "OpenSDLC-Dev/managed-agent-console",
            "ref": CONSOLE_SHA,
            "path": "console-source",
            "persist-credentials": "false",
        })

    def test_tools_and_frozen_browser_install(self):
        self.assertEqual([ref.value.split("@", 1)[0] for ref in self.refs], [
            "actions/checkout", "actions/checkout", "actions/setup-go",
            "pnpm/action-setup", "actions/setup-node",
        ], "no image publisher or artifact upload belongs in this source job")
        for ref in self.refs:
            self.assertIsNone(PINS["violation"](ref))
        self.assertEqual(self.inputs(self.refs[2]), {"go-version-file": "go.mod"})
        self.assertEqual(self.inputs(self.refs[3]), {"version": "11"})
        self.assertEqual(self.inputs(self.refs[4]), {"node-version": "24"})
        blocks = PIPES["run_blocks"](self.lines)
        self.assertEqual(len(blocks), 3, "install, browser install, then native runner")
        self.assertLess(self.refs[-1].line, blocks[0][0], "set up tools before installing")
        self.assertEqual(shlex.split(blocks[0][1]), [
            "pnpm", "--dir", "$GITHUB_WORKSPACE/console-source", "install", "--frozen-lockfile",
        ])
        self.assertEqual(shlex.split(blocks[1][1]), [
            "pnpm", "--dir", "$GITHUB_WORKSPACE/console-source", "exec", "playwright",
            "install", "--with-deps", "chromium",
        ])

    def test_native_command_has_only_ci_allowlist_and_owned_tmpdir(self):
        blocks = PIPES["run_blocks"](self.lines)
        self.assertEqual(len(blocks), 3)
        script = blocks[-1][1].replace("\\\n", "")
        commands = [shlex.split(line, comments=True) for line in script.splitlines()]
        commands = [command for command in commands if command]
        self.assertEqual(len(commands), 2, "create TMPDIR before the native command")
        self.assertEqual(commands[0], ["mkdir", "-p", "$RUNNER_TEMP/native-owned-tmp"])
        launch = commands[1]
        self.assertEqual(launch[:2], ["env", "-i"], "native must not inherit credentials")
        self.assertEqual(launch[-4:], [
            "pnpm", "--dir", "$GITHUB_WORKSPACE/console-source", "test:e2e:native",
        ], "must run the pinned Console's native suite")
        pairs = [argument.split("=", 1) for argument in launch[2:-4]]
        self.assertTrue(all(len(pair) == 2 for pair in pairs), "expected env assignments")
        environment = dict(pairs)
        self.assertEqual(len(environment), len(pairs), "duplicate env assignment")
        self.assertEqual(environment, {
            "PATH": "$PATH", "HOME": "$HOME",
            "TMPDIR": "$RUNNER_TEMP/native-owned-tmp",
            "NEXT_TELEMETRY_DISABLED": "1", "CI": "true",
            "NATIVE_PLATFORM_SOURCE": "$GITHUB_WORKSPACE",
            "NATIVE_EVIDENCE_DIR": "$RUNNER_TEMP/native-owned",
        })
        # shlex removes quotes. Check the variable arguments retain shell quoting,
        # so runner paths with spaces are still single arguments at execution.
        for key, value in environment.items():
            if value.startswith("$"):
                self.assertIn(f'{key}="{value}"', script)
        self.assertIn('mkdir -p "$RUNNER_TEMP/native-owned-tmp"', script)
        self.assertIn('pnpm --dir "$GITHUB_WORKSPACE/console-source"', script)


if __name__ == "__main__":
    unittest.main(verbosity=2)
