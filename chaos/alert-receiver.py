#!/usr/bin/env python3
"""Minimal alert receiver for the chaos harness.

Usage: alert-receiver.py PORT OUTFILE
Appends each POSTed body to OUTFILE, one JSON document per line, and answers 200.
"""
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        with open(sys.argv[2], "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()

    def log_message(self, *_args):
        pass  # keep the harness log readable


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
