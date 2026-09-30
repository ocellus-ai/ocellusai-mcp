#!/usr/bin/env python3
"""Drive ocellusai-mcp over stdio: list tools or call one and print the result.

    python3 scripts/mcp-call.py -config config.stdio.yaml list
    python3 scripts/mcp-call.py -config config.stdio.yaml call node_cpu_anomalies '{"range": "12h"}'
    python3 scripts/mcp-call.py -config config.stdio.yaml call alerts_firing --structured

The proxy's own logs (stderr) are passed through; pass -quiet to drop them.
Only the standard library is used.
"""
import argparse
import json
import subprocess
import sys
import time

PROTOCOL = "2025-06-18"


class Client:
    def __init__(self, binary, config, quiet):
        self.proc = subprocess.Popen(
            [binary, "-config", config],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL if quiet else None,
            text=True,
            bufsize=1,
        )
        self.next_id = 1

    def send(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        if notify:
            self.proc.stdin.write(json.dumps(msg) + "\n")
            self.proc.stdin.flush()
            return None
        msg["id"] = self.next_id
        self.next_id += 1
        self.proc.stdin.write(json.dumps(msg) + "\n")
        self.proc.stdin.flush()
        return msg["id"]

    def wait(self, want_id):
        while True:
            line = self.proc.stdout.readline()
            if not line:
                raise SystemExit("ocellusai-mcp closed stdout (exit %s)" % self.proc.poll())
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue
            if msg.get("id") == want_id:
                if "error" in msg:
                    raise SystemExit("JSON-RPC error: %s" % json.dumps(msg["error"], ensure_ascii=False))
                return msg["result"]

    def initialize(self):
        rid = self.send("initialize", {
            "protocolVersion": PROTOCOL,
            "capabilities": {},
            "clientInfo": {"name": "mcp-call", "version": "0"},
        })
        self.wait(rid)
        self.send("notifications/initialized", notify=True)

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-bin", default="./bin/ocellusai-mcp", help="ocellusai-mcp binary (default ./bin/ocellusai-mcp)")
    ap.add_argument("-config", default="config.yaml", help="config with server.transport: stdio")
    ap.add_argument("-quiet", action="store_true", help="drop the proxy's stderr logs")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("list", help="print tool names and descriptions")
    call = sub.add_parser("call", help="call one tool")
    call.add_argument("tool")
    call.add_argument("arguments", nargs="?", default="{}", help="JSON object with the arguments")
    call.add_argument("--structured", action="store_true", help="also print structuredContent as JSON")
    args = ap.parse_args()

    c = Client(args.bin, args.config, args.quiet)
    try:
        c.initialize()
        if args.cmd == "list":
            res = c.wait(c.send("tools/list", {}))
            for t in res.get("tools", []):
                props = t.get("inputSchema", {}).get("properties", {})
                print("%s  [%s]" % (t["name"], ", ".join(props)))
                print("    " + t.get("description", "").strip().replace("\n", "\n    "))
            return
        try:
            arguments = json.loads(args.arguments)
        except json.JSONDecodeError as e:
            raise SystemExit("arguments must be a JSON object: %s" % e)
        t0 = time.monotonic()
        res = c.wait(c.send("tools/call", {"name": args.tool, "arguments": arguments}))
        dt = time.monotonic() - t0
        status = "ERROR" if res.get("isError") else "ok"
        print("== %s(%s): %s in %.2fs" % (args.tool, json.dumps(arguments, ensure_ascii=False), status, dt))
        for part in res.get("content", []):
            if part.get("type") == "text":
                print(part["text"])
        if args.structured and "structuredContent" in res:
            print("== structuredContent:")
            print(json.dumps(res["structuredContent"], indent=2, ensure_ascii=False))
        if res.get("isError"):
            sys.exit(1)
    finally:
        c.close()


if __name__ == "__main__":
    main()
