"""The binaries under test.

BOWERY_BIN points the suite at any build of the CLI: the tests are
black-box on purpose, so they are the conformance suite for any
implementation. OFFICE_BIN and HOURS_BIN do the same for the office host
and the hours rule plugin; unset, they are found beside BOWERY_BIN (office,
rules/hours, as a release lays them out). With none of them set, the
checkout is built once into a temp directory.
"""

import atexit
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def _build() -> str:
    out = tempfile.mkdtemp(prefix="bowery-test-")
    atexit.register(shutil.rmtree, out, ignore_errors=True)
    env = {**os.environ, "CGO_ENABLED": "0"}
    for name, pkg in (("bowery", "./cmd/bowery"), ("office", "./cmd/office"), ("rules/hours", "./cmd/rules/hours")):
        subprocess.run(["go", "build", "-o", os.path.join(out, name), pkg], cwd=ROOT, check=True, env=env)
    return os.path.join(out, "bowery")


BIN = os.environ.get("BOWERY_BIN") or _build()
_dir = os.path.dirname(BIN)
OFFICE_BIN = os.environ.get("OFFICE_BIN") or os.path.join(_dir, "office")
HOURS_BIN = os.environ.get("HOURS_BIN") or os.path.join(_dir, "rules", "hours")
