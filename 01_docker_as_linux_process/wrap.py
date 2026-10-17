#!/usr/bin/env python3
import errno
import os
import sys

import seccomp

f = seccomp.SyscallFilter(
    defaction=seccomp.ALLOW
)  # denylist: allow everything by default
f.add_rule(seccomp.ERRNO(errno.EPERM), "settimeofday")  # what your Go handler uses
f.load()  # installs the filter, sets no_new_privs
os.execvp(sys.argv[1], sys.argv[1:])
