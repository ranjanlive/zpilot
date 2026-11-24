"""Example tool that produces a file. Pure standard library (writes a PNG by hand).

zagent gives every call a private folder in $ZAGENT_OUTPUT_DIR. Whatever the tool writes there is listed in the
result, and because the manifest sets "attach_outputs": true the picture is also shown to the model (if the model
supports images).
"""
import json
import os
import struct
import sys
import zlib

args = json.load(sys.stdin)
color = args["color"].lstrip("#")
if len(color) != 6 or any(c not in "0123456789abcdefABCDEF" for c in color):
    print(json.dumps({"output": f"'{args['color']}' is not a hex colour like #3366cc", "is_error": True}))
    sys.exit(0)
size = int(args.get("size", 128))
r, g, b = (int(color[i:i + 2], 16) for i in (0, 2, 4))

rows = []
for y in range(size):
    row = bytearray([0])  # filter type 0 for each scanline
    for x in range(size):
        stripe = abs(x - y) < size // 12
        row += bytes((r // 2, g // 2, b // 2) if stripe else (r, g, b))
    rows.append(bytes(row))

def chunk(kind, data):
    body = kind + data
    return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 2, 0, 0, 0)) \
    + chunk(b"IDAT", zlib.compress(b"".join(rows))) + chunk(b"IEND", b"")
path = os.path.join(os.environ["ZAGENT_OUTPUT_DIR"], "image.png")
with open(path, "wb") as f:
    f.write(png)
print(json.dumps({"output": f"created a {size}x{size} image filled with #{color}"}))
