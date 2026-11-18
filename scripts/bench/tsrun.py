#!/usr/bin/env python3
"""Run a command and prefix each output line with seconds since start. Prints the total time."""
import subprocess
import sys
import time

start = time.monotonic()
p = subprocess.Popen(sys.argv[1:], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=1, text=True)
for line in p.stdout:
    sys.stdout.write(f"{time.monotonic() - start:7.2f} {line}")
    sys.stdout.flush()
code = p.wait()
print(f"TOTAL {time.monotonic() - start:.2f}s exit {code}")
sys.exit(code)
