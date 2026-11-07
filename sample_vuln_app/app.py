"""Tiny deliberately-vulnerable demo service for smoke-testing the auditor."""
import os
import sqlite3
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

DB = "demo.db"


def init_db():
    conn = sqlite3.connect(DB)
    conn.execute("CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name TEXT, role TEXT)")
    conn.execute("INSERT OR IGNORE INTO users (id, name, role) VALUES (1, 'admin', 'admin')")
    conn.commit()
    conn.close()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        url = urlparse(self.path)
        params = parse_qs(url.query)
        if url.path == "/user":
            name = params.get("name", [""])[0]
            conn = sqlite3.connect(DB)
            # Deliberate SQL injection: user input is concatenated into SQL.
            row = conn.execute("SELECT id, name, role FROM users WHERE name = '%s'" % name).fetchone()
            conn.close()
            self.send_response(200)
            self.end_headers()
            self.wfile.write(repr(row).encode())
        elif url.path == "/ping":
            host = params.get("host", ["127.0.0.1"])[0]
            # Deliberate command injection: user input reaches the shell.
            output = os.popen("ping -c 1 " + host).read()
            self.send_response(200)
            self.end_headers()
            self.wfile.write(output.encode())
        else:
            self.send_response(404)
            self.end_headers()


if __name__ == "__main__":
    init_db()
    HTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
