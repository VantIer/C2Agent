"""ChaCha20 stream cipher - pure Python, no third-party dependencies.

Adapted from the reference implementation in ``样例代码/chacha20.c``
(CycloneCRYPTO Open, RFC 7539 semantics: 256-bit key, 96-bit nonce,
32-bit block counter).

After the registration handshake succeeds, ALL packet traffic is encrypted
with ChaCha20. Both endpoints derive the 32-byte key AND the 12-byte nonce
per connection from the fresh handshake nonce and the shared auth token, so
every connection uses a one-time key+nonce pair:

  key(dir)   = SHA-256(nonce || token || nonce || dir)          (32 bytes)
  nonce(dir) = SHA-256(token || nonce || token || dir)[0:12]    (12 bytes)

``dir`` separates the two directions (DIR_C2_TO_AGENT / DIR_AGENT_TO_C2) so
they never share a keystream.

``EncryptedStream`` transparently encrypts writes / decrypts reads on top
of an asyncio reader/writer pair, so the rest of the code keeps using the
familiar ``write()`` / ``drain()`` / ``read()`` interface.
"""

import hashlib
import struct
from typing import Tuple

DIR_C2_TO_AGENT = 0x01
DIR_AGENT_TO_C2 = 0x02


def derive_material(auth_token: str, handshake_nonce: str, direction: int) -> Tuple[bytes, bytes]:
    """Derive the per-connection ChaCha20 (key, nonce) for one direction.

    key   = SHA-256(nonce || token || nonce || dir)          (32 bytes)
    nonce = SHA-256(token || nonce || token || dir)[0:12]    (12 bytes)
    """
    token = auth_token.encode("utf-8")
    nonce = handshake_nonce.encode("utf-8")
    d = bytes([direction & 0xFF])
    key = hashlib.sha256(nonce + token + nonce + d).digest()
    n = hashlib.sha256(token + nonce + token + d).digest()[:12]
    return key, n


class ChaCha20:
    """RFC 7539 ChaCha20 stream cipher (256-bit key, 96-bit nonce)."""

    def __init__(self, key: bytes, nonce: bytes, initial_counter: int = 0):
        if len(key) != 32:
            raise ValueError("ChaCha20 requires a 32-byte key")
        if len(nonce) != 12:
            raise ValueError("ChaCha20 requires a 12-byte nonce")
        constants = (0x61707865, 0x3320646E, 0x79622D32, 0x6B206574)
        key_words = struct.unpack("<8I", key)
        nonce_words = struct.unpack("<3I", nonce)
        self._state = (
            list(constants)
            + list(key_words)
            + [initial_counter & 0xFFFFFFFF]
            + list(nonce_words)
        )
        self._keystream = bytearray(64)
        self._pos = 0

    @staticmethod
    def _rol(x, n):
        return ((x << n) | (x >> (32 - n))) & 0xFFFFFFFF

    def _block(self):
        state = self._state
        w = list(state)

        def qr(a, b, c, d):
            w[a] = (w[a] + w[b]) & 0xFFFFFFFF
            w[d] = self._rol(w[d] ^ w[a], 16)
            w[c] = (w[c] + w[d]) & 0xFFFFFFFF
            w[b] = self._rol(w[b] ^ w[c], 12)
            w[a] = (w[a] + w[b]) & 0xFFFFFFFF
            w[d] = self._rol(w[d] ^ w[a], 8)
            w[c] = (w[c] + w[d]) & 0xFFFFFFFF
            w[b] = self._rol(w[b] ^ w[c], 7)

        for _ in range(10):
            qr(0, 4, 8, 12)
            qr(1, 5, 9, 13)
            qr(2, 6, 10, 14)
            qr(3, 7, 11, 15)
            qr(0, 5, 10, 15)
            qr(1, 6, 11, 12)
            qr(2, 7, 8, 13)
            qr(3, 4, 9, 14)

        self._keystream[:] = struct.pack(
            "<16I", *((w[i] + state[i]) & 0xFFFFFFFF for i in range(16))
        )
        state[12] = (state[12] + 1) & 0xFFFFFFFF
        if state[12] == 0:
            state[13] = (state[13] + 1) & 0xFFFFFFFF
        self._pos = 0

    def crypt(self, data: bytes) -> bytes:
        n = len(data)
        out = bytearray(n)
        offset = 0
        while offset < n:
            if self._pos == 0 or self._pos >= 64:
                self._block()
            take = min(n - offset, 64 - self._pos)
            ks = self._keystream
            pos = self._pos
            for i in range(take):
                out[offset + i] = data[offset + i] ^ ks[pos + i]
            self._pos += take
            offset += take
        return bytes(out)


class EncryptedStream:
    """Transparently encrypts writes / decrypts reads on a connection.

    Intended for post-handshake traffic only. ``write`` is synchronous
    (like ``asyncio.StreamWriter.write``), ``drain`` is async, ``read`` is
    async and returns decrypted bytes.
    """

    def __init__(self, reader, writer, tx: ChaCha20, rx: ChaCha20):
        self._reader = reader
        self._writer = writer
        self._tx = tx
        self._rx = rx

    def write(self, data: bytes) -> None:
        self._writer.write(self._tx.crypt(data))

    async def drain(self) -> None:
        await self._writer.drain()

    async def read(self, n: int = -1) -> bytes:
        if n < 0:
            raw = await self._reader.read()
        else:
            raw = await self._reader.read(n)
        if not raw:
            return raw
        return self._rx.crypt(raw)

    def absorb_leftover(self, pr) -> None:
        """Decrypt and re-feed bytes buffered in a PacketReader before
        encryption was enabled.

        After the handshake the peer may already have sent the first
        encrypted bytes (e.g. TCP coalescing with the register_confirm).
        Those raw bytes must be decrypted with the rx cipher before the
        packet reader parses them.
        """
        if pr.buffered:
            raw = pr.drain_all()
            pr.feed(self._rx.crypt(raw))
