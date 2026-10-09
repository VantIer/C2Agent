"""Self-test for the Python agent's exact-string edit_file.

Run:  python remote/remote-py/test_local_executor.py
"""

import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import local_executor  # noqa: E402


class EditFileTest(unittest.TestCase):
    def _write(self, data: bytes) -> str:
        fd, path = tempfile.mkstemp()
        os.close(fd)
        with open(path, "wb") as f:
            f.write(data)
        self.addCleanup(lambda: os.path.exists(path) and os.remove(path))
        return path

    def test_unique_multiline_replace(self):
        path = self._write(b"a\nb\nc\n")
        out = local_executor.edit_file(path, "b\n", "x\ny\n")
        self.assertTrue(out.startswith("Successfully"), out)
        with open(path, "rb") as f:
            self.assertEqual(f.read(), b"a\nx\ny\nc\n")

    def test_delete(self):
        path = self._write(b"keep\ndrop\nkeep2\n")
        out = local_executor.edit_file(path, "drop\n", "")
        self.assertTrue(out.startswith("Successfully"), out)
        with open(path, "rb") as f:
            self.assertEqual(f.read(), b"keep\nkeep2\n")

    def test_not_found(self):
        path = self._write(b"a\nb\n")
        self.assertTrue(local_executor.edit_file(path, "zzz", "y").startswith("Error:"))

    def test_ambiguous(self):
        path = self._write(b"x\nx\n")
        self.assertTrue(local_executor.edit_file(path, "x\n", "y\n").startswith("Error:"))

    def test_empty_old_text(self):
        path = self._write(b"a\n")
        self.assertTrue(local_executor.edit_file(path, "", "y").startswith("Error:"))

    def test_overlapping_is_ambiguous(self):
        path = self._write(b"aaa")
        self.assertTrue(local_executor.edit_file(path, "aa", "b").startswith("Error:"))

    def test_crlf_tolerant_and_preserved(self):
        path = self._write(b"a\r\nb\r\nc\r\n")
        out = local_executor.edit_file(path, "b\n", "x\n")
        self.assertTrue(out.startswith("Successfully"), out)
        with open(path, "rb") as f:
            self.assertEqual(f.read(), b"a\r\nx\r\nc\r\n")

    def test_first_line_looks_like_error(self):
        # Content starting with "Error:" must still edit (no false read failure).
        path = self._write(b"Error: pretend\nsecond\n")
        out = local_executor.edit_file(path, "second\n", "third\n")
        self.assertTrue(out.startswith("Successfully"), out)


if __name__ == "__main__":
    unittest.main()
