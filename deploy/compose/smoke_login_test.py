#!/usr/bin/env python3
"""Setup discovers the boot file from real slog output, never a guessed path."""
import unittest
from smoke_login import setup_path


class SetupPath(unittest.TestCase):
    def test_reads_text_and_json_startup_logs(self):
        path = "/tmp/map-setup-123456/setup-code"
        self.assertEqual(setup_path('time=2026-11-16 level=WARN msg="no active local admin exists" setup_code_file=' + path + ' expires_in=1h'), path)
        self.assertEqual(setup_path('{"setup_code_file":"' + path + '"}'), path)
        self.assertEqual(setup_path('other log\nsetup_code_file=/tmp/map-setup-1/setup-code\nsetup_code_file=' + path), path)

    def test_refuses_absent_or_unexpected_paths(self):
        for logs in ['', 'setup_code_file=/etc/passwd', 'setup_code_file=/tmp/map-setup-1/setup-code/../other', '{"setup_code_file":null}', '{"setup_code_file":123}', '[]']:
            with self.subTest(logs=logs), self.assertRaises(RuntimeError):
                setup_path(logs)


if __name__ == "__main__":
    unittest.main(verbosity=2)
