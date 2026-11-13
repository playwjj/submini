#!/usr/bin/env python3
"""The player page runs on iOS 9 Safari: hand-written ES5 only.

Extract web/mini.html's inline script, reject post-ES5 tokens, and
syntax-check it with node --check. One missed arrow function ships a
page the iPad cannot run at all, so this runs in CI on every push.
"""
import os
import re
import subprocess
import sys
import tempfile

FORBIDDEN = ["=>", "const ", "let ", "`", "fetch(", "Promise",
             "class ", "async ", "await ", "..."]


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "web/mini.html"
    html = open(path, encoding="utf-8").read()
    m = re.search(r"<script>(.*)</script>", html, re.S)
    if not m:
        print("check_page: no inline <script> found in", path)
        return 1
    js = m.group(1)
    bad = [t for t in FORBIDDEN if t in js]
    if bad:
        print("check_page: post-ES5 tokens in page script:", bad)
        return 1
    fd, tmp = tempfile.mkstemp(suffix=".js")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(js)
        r = subprocess.run(["node", "--check", tmp],
                           capture_output=True, text=True)
        if r.returncode != 0:
            print("check_page: node --check failed:")
            print(r.stdout)
            print(r.stderr)
            return 1
    finally:
        os.unlink(tmp)
    print("check_page: ES5 + syntax OK (%d bytes of script)" % len(js))
    return 0


if __name__ == "__main__":
    sys.exit(main())
