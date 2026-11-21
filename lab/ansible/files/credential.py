#!/usr/bin/python3
"""Makes one of the lab's credential files once, private to you (0600), and never changes a file that exists.

Usage: credential.py s3 <file> <access key prefix>   AWS_ACCESS_KEY_ID= and AWS_SECRET_ACCESS_KEY= lines, the format
                                                     storage.ReadKeyFile reads and a shell can source
       credential.py password <file>                 one line: a password

Keys and secrets are letters and digits only, so they need no escaping in a URL, a shell or a SQL literal.
"""
import os
import secrets
import string
import sys


def random(alphabet, n):
    return "".join(secrets.choice(alphabet) for _ in range(n))


def main():
    kind, path = sys.argv[1], sys.argv[2]
    if os.path.exists(path):
        return 0
    if kind == "s3":
        prefix = sys.argv[3].upper()
        key = prefix + random(string.ascii_uppercase + string.digits, 20 - len(prefix))
        secret = random(string.ascii_letters + string.digits, 40)
        text = f"AWS_ACCESS_KEY_ID={key}\nAWS_SECRET_ACCESS_KEY={secret}\n"
    elif kind == "password":
        text = random(string.ascii_letters + string.digits, 32) + "\n"
    else:
        print(f"credential.py: no kind {kind!r}", file=sys.stderr)
        return 2
    tmp = f"{path}.{os.getpid()}"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(text)
    os.rename(tmp, path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
