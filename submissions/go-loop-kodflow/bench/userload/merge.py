#!/usr/bin/env python3
"""Merges the raw duration files of several userload processes.

    python3 -I merge.py <prefix>...

Each prefix names one process's <prefix>.all and <prefix>.hold (userload
-raw). Prints, for the whole run and the hold alone, the requests, failures
and duration percentiles of all processes together, and the k6 verdict
(p95 < 500 ms, p99 < 1 s, failures < 1 %) on the whole run.
"""
import array
import json
import sys


def load(path):
    with open(path, "rb") as f:
        b = f.read()
    durs = array.array("I")
    durs.frombytes(b[:-16])
    tail = array.array("Q")
    tail.frombytes(b[-16:])
    return durs, tail[0], tail[1]


def phase(prefixes, ext):
    durs, reqs, fails = array.array("I"), 0, 0
    for p in prefixes:
        d, r, f = load(p + ext)
        durs.extend(d)
        reqs += r
        fails += f
    s = sorted(durs)

    def pct(q):
        return s[int(q * (len(s) - 1))] / 10 if s else 0

    return {
        "reqs": reqs, "fails": fails, "fail_rate": fails / reqs if reqs else 0,
        "p50_ms": pct(0.5), "p95_ms": pct(0.95), "p99_ms": pct(0.99), "max_ms": pct(1),
    }


def main():
    prefixes = sys.argv[1:]
    res = {"all": phase(prefixes, ".all"), "hold": phase(prefixes, ".hold")}
    a = res["all"]
    res["pass"] = a["p95_ms"] < 500 and a["p99_ms"] < 1000 and a["fail_rate"] < 0.01
    print(json.dumps(res, indent=2))


main()
