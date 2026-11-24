"""Example background tool: reports progress while it works.

Environment provided by zagent (see docs/EXTERNAL_TOOLS.md):
  ZAGENT_OUTPUT_DIR  a private folder for files this call produces
  ZAGENT_NOTIFY_FILE append a line here to send the running agent a note;
                     a JSON line {"text": "...", "wake": true} also wakes an idle agent
"""
import json
import os
import sys
import time

args = json.load(sys.stdin)
topic = args["topic"]
seconds = int(args.get("seconds", 6))
notify = os.environ["ZAGENT_NOTIFY_FILE"]
out_dir = os.environ["ZAGENT_OUTPUT_DIR"]

def note(text, wake=False):
    with open(notify, "a", encoding="utf-8") as f:
        f.write(json.dumps({"text": text, "wake": wake}) + "\n")

for step in range(1, 4):
    time.sleep(seconds / 3)
    note(f"slow_report on '{topic}': step {step} of 3 done")

path = os.path.join(out_dir, "report.md")
with open(path, "w", encoding="utf-8") as f:
    f.write(f"# Report: {topic}\n\nGenerated in {seconds}s. Findings: all systems nominal.\n")
print(json.dumps({"output": f"report on '{topic}' is ready at {path}"}))
