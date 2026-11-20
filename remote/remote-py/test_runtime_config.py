import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from runtime_config import MAX_TIMING_SECONDS, RuntimeConfig  # noqa: E402


class RuntimeConfigTest(unittest.TestCase):
    def test_update_in_range(self):
        rc = RuntimeConfig()
        rc.update(40, 15)
        self.assertEqual(rc.heartbeat_interval(), 40)
        self.assertEqual(rc.cmd_timeout(), 15)

    def test_out_of_range_values_are_ignored(self):
        rc = RuntimeConfig(heartbeat_interval=30, cmd_timeout=60)
        rc.update(MAX_TIMING_SECONDS + 1, 0)
        self.assertEqual(rc.heartbeat_interval(), 30)
        self.assertEqual(rc.cmd_timeout(), 60)

    def test_maximum_is_accepted(self):
        rc = RuntimeConfig()
        rc.update(MAX_TIMING_SECONDS, MAX_TIMING_SECONDS)
        self.assertEqual(rc.heartbeat_interval(), MAX_TIMING_SECONDS)
        self.assertEqual(rc.cmd_timeout(), MAX_TIMING_SECONDS)


if __name__ == "__main__":
    unittest.main()
