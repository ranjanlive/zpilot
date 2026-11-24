"""Example zagent tool: text statistics.

Protocol (see docs/EXTERNAL_TOOLS.md): the call arguments arrive as one JSON
object on stdin; because the manifest says "output": "json", the result is a
JSON object on stdout. Anything printed to stderr is kept as diagnostics.
"""
import collections
import json
import re
import sys

args = json.load(sys.stdin)
text = args["text"]
top = args.get("top", 5)

words = re.findall(r"[\w']+", text.lower())
common = collections.Counter(words).most_common(top)
lines = text.count("\n") + (1 if text and not text.endswith("\n") else 0)

report = [
    f"words: {len(words)}",
    f"lines: {lines}",
    f"characters: {len(text)}",
    "most frequent: " + ", ".join(f"{w} ({n})" for w, n in common),
]
print(json.dumps({"output": "\n".join(report)}))
