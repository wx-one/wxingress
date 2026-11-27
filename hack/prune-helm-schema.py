#!/usr/bin/env python3
"""Keeps only the Kubernetes definitions of a Helm values schema that it refers to.

The chart's values.schema.json carries the Kubernetes definitions so that Helm
validates values without network access (hack/generate-helm-schema.sh). All of
them are 1.4 MB: with the CRDs in the chart, the release Secret Helm stores
grows past Kubernetes' 1 MiB limit and the install fails. The definitions the
schema refers to, and those they refer to, are a tenth of that.

    hack/prune-helm-schema.py charts/wxingress/values.schema.json
"""
import json
import re
import sys

REF = re.compile(r'#/definitions/([^/"]+)')

path = sys.argv[1]
with open(path) as f:
    schema = json.load(f)
definitions = schema.get("definitions", {})
rest = json.dumps({k: v for k, v in schema.items() if k != "definitions"})

keep, todo = set(), REF.findall(rest)
while todo:
    name = todo.pop()
    if name in keep or name not in definitions:
        continue
    keep.add(name)
    todo.extend(REF.findall(json.dumps(definitions[name])))

missing = sorted({n for n in REF.findall(rest) if n not in definitions})
if missing:
    sys.exit(f"{path}: refers to definitions it does not have: {', '.join(missing)}")

schema["definitions"] = {k: definitions[k] for k in sorted(keep)}
with open(path, "w") as f:
    json.dump(schema, f, indent=2)
    f.write("\n")
print(f"{path}: {len(keep)} of {len(definitions)} definitions kept")
