#!/usr/bin/python3
"""s3req: one AWS SigV4-signed HTTP request to RGW (S3, the Admin Ops API or IAM), for the lab's experiments.

Usage: s3req [--service s3|iam] [--region R] [--data FILE|-] [--header 'Name: value']... [--show-header NAME]...
             [--body] METHOD URL

The key comes from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY in the environment, never from argv. It prints one
line, "METHOD PATH -> STATUS [ErrorCode]", with any secret-key= value in the query shown as ***, then each response
header --show-header names ("Name: value"), then with --body the response body, with every "secret_key" in it shown
as ***. It exits 0 for a 2xx status and 1 otherwise. botocore (python3-botocore) signs the request; the request itself
goes out with http.client, exactly as signed.
"""
import argparse
import http.client
import os
import re
import sys
import urllib.parse

from botocore.auth import S3SigV4Auth, SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.credentials import Credentials


def main():
    ap = argparse.ArgumentParser(prog="s3req")
    ap.add_argument("--service", default="s3")
    ap.add_argument("--region", default=os.environ.get("AWS_DEFAULT_REGION", "default"))
    ap.add_argument("--data")
    ap.add_argument("--header", action="append", default=[])
    ap.add_argument("--show-header", action="append", default=[])
    ap.add_argument("--body", action="store_true")
    ap.add_argument("method")
    ap.add_argument("url")
    a = ap.parse_args()

    data = b""
    if a.data == "-":
        data = sys.stdin.buffer.read()
    elif a.data:
        with open(a.data, "rb") as f:
            data = f.read()
    headers = {}
    for h in a.header:
        k, v = h.split(":", 1)
        headers[k.strip()] = v.strip()

    # The query exactly as SigV4 canonicalises it (each name and value percent-encoded but for A-Z a-z 0-9 - _ . ~),
    # so that what is signed (botocore signs the query as it is given) and what RGW recomputes agree: a "/" in
    # prefix= would otherwise fail with SignatureDoesNotMatch. A name without "=" (?policy, ?key) stays so.
    u = urllib.parse.urlsplit(a.url)
    q = lambda s: urllib.parse.quote(urllib.parse.unquote(s), safe="-_.~")
    query = "&".join(
        q(k) + (eq + q(v) if eq else "") for k, eq, v in (p.partition("=") for p in u.query.split("&") if p)
    )
    url = urllib.parse.urlunsplit((u.scheme, u.netloc, u.path or "/", query, ""))

    creds = Credentials(os.environ["AWS_ACCESS_KEY_ID"], os.environ["AWS_SECRET_ACCESS_KEY"])
    req = AWSRequest(method=a.method, url=url, data=data, headers=headers)
    signer = S3SigV4Auth if a.service == "s3" else SigV4Auth
    signer(creds, a.service, a.region).add_auth(req)

    path = (u.path or "/") + ("?" + query if query else "")
    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=60)
    conn.request(a.method, path, body=data or None, headers=dict(req.headers.items()))
    r = conn.getresponse()
    body = r.read()

    m = re.search(rb"<Code>([^<]+)</Code>", body) or re.search(rb'"Code"\s*:\s*"([^"]+)"', body)
    code = (" " + m.group(1).decode()) if m and r.status >= 300 else ""
    shown = re.sub(r"(secret-key=)[^&]*", r"\1***", urllib.parse.unquote(path))
    print(f"{a.method} {shown} -> {r.status}{code}")
    for name in a.show_header:
        print(f"{name}: {r.getheader(name, '')}")
    if a.body and body:
        text = body.decode(errors="replace")
        print(re.sub(r'("secret_key"\s*:\s*")[^"]*', r"\1***", text))
    return 0 if 200 <= r.status < 300 else 1


if __name__ == "__main__":
    sys.exit(main())
