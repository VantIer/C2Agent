"""Shared resource limits for the Python controlled end."""

MAX_OUTPUT_BYTES = 8 * 1024 * 1024
OUTPUT_TRUNCATED = "\n[... output truncated at 8 MiB]"
MAX_CONTENT_BYTES = MAX_OUTPUT_BYTES - len(OUTPUT_TRUNCATED.encode("utf-8"))
MAX_READ_FILE_LINES = 512
