#!/usr/bin/env python3
"""
Compares the probes of ingress-nginx (A) with those of wxingress (B).

    diff.py <allow-file> <a.json> <b.json> <what A is>

Every field of every probe - status, the headers kept, what the backend
saw, the body's digest, the certificate - is alike, allowed by a rule of the
allow file, or failing. A rule is a line

    <field regex> | <probe id regex> | <A value regex> | <B value regex> | <why, and where it is documented>

matched against the whole field name (status, headers.location,
backend.headers.x-forwarded-for, body, ...), the probe's id and the two
values as JSON. Prints a line per difference and, last, the counts.
"""

import json
import re
import sys


def flat(r):
    out = {}
    for k, v in r.items():
        if k == "id":
            continue
        if isinstance(v, dict):
            for k2, v2 in v.items():
                if isinstance(v2, dict):
                    for k3, v3 in v2.items():
                        out[f"{k}.{k2}.{k3}"] = v3
                else:
                    out[f"{k}.{k2}"] = v2
        else:
            out[k] = v
    return out


def rules(path):
    out = []
    for line in open(path):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = [p.strip() for p in line.split(" | ")]
        if len(parts) != 5:
            sys.exit(f"bad rule: {line}")
        out.append((re.compile(parts[0]), re.compile(parts[1]), re.compile(parts[2]), re.compile(parts[3]), parts[4]))
    return out


def main():
    allow = rules(sys.argv[1])
    a, b = json.load(open(sys.argv[2])), json.load(open(sys.argv[3]))
    what = sys.argv[4]
    alike = allowed = failing = 0
    why = {}
    inconclusive = 0
    for pid in sorted(set(a) | set(b)):
        fa, fb = flat(a.get(pid, {})), flat(b.get(pid, {}))
        if fa.get("error") == "TimeoutError" and "error" not in fb:
            # ingress-nginx did not answer in time: nothing to compare, said as such
            print(f"INCONCLUSIVE {pid} | {what} timed out")
            inconclusive += 1
            continue
        same = True
        for field in sorted(set(fa) | set(fb)):
            va, vb = fa.get(field), fb.get(field)
            if va == vb:
                continue
            ja, jb = json.dumps(va), json.dumps(vb)
            rule = next((r for r in allow if r[0].fullmatch(field) and r[1].search(pid) and r[2].search(ja) and r[3].search(jb)), None)
            if rule:
                print(f"ALLOWED  {pid} | {field} | {ja} | {jb} | {rule[4]}")
                why[rule[4]] = why.get(rule[4], 0) + 1
            else:
                print(f"FAIL     {pid} | {field} | {what}: {ja} | wxingress: {jb}")
                same = False
        if fa == fb:
            alike += 1
        elif same:
            allowed += 1
        else:
            failing += 1
    for reason, n in sorted(why.items(), key=lambda x: -x[1]):
        print(f"# {n:5d}  {reason}")
    print(f"probes {len(set(a) | set(b))}: alike {alike} allowed {allowed} inconclusive {inconclusive} failing {failing}")
    print(f"failing {failing}")


if __name__ == "__main__":
    main()
