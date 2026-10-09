"""Self-test for the shared ChaCha20 key/nonce derivation.

Run:  python remote/common/test_crypto.py
The expected values are the cross-language interop vectors shared with the Go
control end (internal/crypto) and remote-go.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from crypto import DIR_AGENT_TO_C2, DIR_C2_TO_AGENT, derive_material  # noqa: E402


class DeriveMaterialTest(unittest.TestCase):
    TOKEN = "change-me-shared-token"
    NONCE = "0123456789abcdef"

    def test_interop_vectors(self):
        cases = [
            (DIR_C2_TO_AGENT,
             "ea42424fcb14a1a3395d2f013866be2c1c8ab812b8ecd29790f6bda98c4a3dbe",
             "e6d636597a46b43b686f7aa7"),
            (DIR_AGENT_TO_C2,
             "a652964193b67a6d0d2b0787e116c2f05133a1987fb5536df1c4c1f87a64f2ad",
             "5d88656aeb8a77320e65609f"),
        ]
        for direction, want_key, want_nonce in cases:
            key, nonce = derive_material(self.TOKEN, self.NONCE, direction)
            self.assertEqual(key.hex(), want_key)
            self.assertEqual(nonce.hex(), want_nonce)

    def test_directions_and_nonces_differ(self):
        k1, n1 = derive_material(self.TOKEN, self.NONCE, DIR_C2_TO_AGENT)
        k2, n2 = derive_material(self.TOKEN, self.NONCE, DIR_AGENT_TO_C2)
        self.assertNotEqual(k1, k2)
        self.assertNotEqual(n1, n2)
        k3, n3 = derive_material(self.TOKEN, "fedcba9876543210", DIR_C2_TO_AGENT)
        self.assertNotEqual(k1, k3)
        self.assertNotEqual(n1, n3)


if __name__ == "__main__":
    unittest.main()
