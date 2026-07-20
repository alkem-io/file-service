#!/usr/bin/env python3
"""Retype error-response bodies from application/octet-stream to application/json.

The apispec generator infers a binary endpoint's whole response set from its 200 success
Content-Type, so it mistypes the 4xx/5xx JSON error bodies (writeJSONError always sends
application/json with the http.errorResponse schema) as application/octet-stream. This walks the
generated spec and retypes ONLY the media-type lines that carry the errorResponse schema, leaving
the binary 200 success bodies untouched. Run as a post-step of `make openapi` so the committed spec
matches generation (the CI openapi-drift gate stays green).
"""
import re
import sys

path = sys.argv[1]
text = open(path).read()
# An `application/octet-stream:` immediately followed by `schema: $ref …/http.errorResponse` is a
# JSON error body the generator mistyped; success (binary) bodies have a different/no schema here.
pattern = re.compile(
    r"( +)application/octet-stream:(\n +schema:\n +\$ref: ['\"]#/components/schemas/http\.errorResponse['\"])"
)
fixed, n = pattern.subn(r"\1application/json:\2", text)
open(path, "w").write(fixed)
print(f"openapi: retyped {n} error-body media type(s) octet-stream -> json")
