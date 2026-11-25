"""Signing keys for the offices tests: one ed25519 key per principal and per
region, made once per run with ssh-keygen, and signing with ssh-keygen -Y,
so the tests check the office against the real tool's signatures.
"""

import atexit
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

_dir = None
_keys = {}


def _root():
    global _dir
    if _dir is None:
        _dir = Path(tempfile.mkdtemp(prefix="offices-keys-"))
        atexit.register(shutil.rmtree, _dir, True)
    return _dir


def have_ssh_keygen():
    return shutil.which("ssh-keygen") is not None


def key(name):
    """The private key file for name (made on first use)."""
    if name not in _keys:
        p = _root() / name
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", str(p)],
                       check=True, capture_output=True)
        os.chmod(p, 0o600)
        _keys[name] = p
    return _keys[name]


def pub(name):
    """`ssh-ed25519 <base64>` for name."""
    return " ".join(Path(str(key(name)) + ".pub").read_text().split()[:2])


def sign(name, namespace, data):
    """An armored ssh-keygen -Y signature by name's key over data."""
    r = subprocess.run(["ssh-keygen", "-q", "-Y", "sign", "-f", str(key(name)), "-n", namespace],
                       input=data.encode(), capture_output=True, check=True)
    return r.stdout.decode()


def letter(block, requester=None, region=None):
    """A letter: the block, the requester's signature, a region's seal."""
    out = block
    if requester:
        out += "```signature requester\n" + sign(requester, "placement", block).rstrip("\n") + "\n```\n"
    if region:
        out += "```signature region\n" + sign(region, "placement-region", block).rstrip("\n") + "\n```\n"
    return out
