"""Runtime timing supplied by the C2 registration ACK.

The C2 sends ``heartbeat_interval_sec`` / ``cmd_timeout`` in ``register_ack``
immediately after ``register_confirm``. This object stores them behind a lock
so the heartbeat task and the command dispatcher always see a consistent pair.
"""

import threading

# Upper bound for timing values accepted from register_ack.
MAX_TIMING_SECONDS = 600


class RuntimeConfig:
    def __init__(self, heartbeat_interval: int = 30, cmd_timeout: int = 60) -> None:
        self._lock = threading.Lock()
        self._heartbeat_interval = max(1, int(heartbeat_interval or 30))
        self._cmd_timeout = max(1, int(cmd_timeout or 60))

    def update(self, heartbeat_interval: int, cmd_timeout: int) -> None:
        """Apply a register_ack payload. Out-of-range values are ignored."""
        with self._lock:
            if 0 < heartbeat_interval <= MAX_TIMING_SECONDS:
                self._heartbeat_interval = int(heartbeat_interval)
            if 0 < cmd_timeout <= MAX_TIMING_SECONDS:
                self._cmd_timeout = int(cmd_timeout)

    def heartbeat_interval(self) -> int:
        with self._lock:
            return self._heartbeat_interval

    def cmd_timeout(self) -> int:
        with self._lock:
            return self._cmd_timeout
