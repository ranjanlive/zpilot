// The call arguments arrive as one JSON object on stdin; the result is one JSON object on stdout.
let input = "";
process.stdin.on("data", (chunk) => (input += chunk)).on("end", () => {
  const args = JSON.parse(input);
  console.log(JSON.stringify({ output: `Hello, ${args.name}! (from Node ${process.version})` }));
});
