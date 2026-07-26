#!/usr/bin/env python3
"""Minimal OpenAI-compatible /chat/completions stub for the chaos harness.

Lets scenario 14 exercise the full on-incident explain path end to end with no
API key anywhere in the shell. Every request is logged one-per-line to the given
file so the scenario can count the calls the daemon actually made.

Usage: explain-stub.py PORT LOGFILE
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
LOGFILE = sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
            user = next(
                (m.get("content", "") for m in reversed(body.get("messages", []))
                 if m.get("role") == "user"),
                "",
            )
        except (ValueError, AttributeError):
            user = ""
        with open(LOGFILE, "a", encoding="utf-8") as fh:
            fh.write(json.dumps({"path": self.path, "prompt": user}) + "\n")

        payload = json.dumps({
            "choices": [{"message": {
                "role": "assistant",
                "content": "Cause: the deployment references an image tag that does not exist.\n"
                           "Check: kubectl -n NS describe deploy/NAME\n"
                           "Fix: kubectl -n NS set image deploy/NAME container=<known-good-tag>",
            }}]
        }).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args):
        pass


HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
