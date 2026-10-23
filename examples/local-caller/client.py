#!/usr/bin/env python3
"""Thin local caller: the existing Agent Engine API owns admission and execution."""

import argparse
import contextlib
import ipaddress
import json
import re
from pathlib import Path
import stat
import sys
import os
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        raise ValueError("local API redirects are refused")


def save_cursor(path, frame):
    if path is None:
        return
    if not path.is_absolute() or path.is_symlink() or not path.parent.is_dir():
        raise ValueError("cursor file requires an existing operator-owned absolute parent")
    descriptor, temporary = tempfile.mkstemp(prefix=".emjin-observer-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(frame, output)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)


def clear_request_cursor(request):
    """Forget only the replay cursor; the episode itself is never resubmitted."""
    for headers in (request.headers, request.unredirected_hdrs):
        for name in list(headers):
            if name.lower() == "last-event-id":
                del headers[name]


def valid_frame(frame, episode_id):
    return (isinstance(frame, dict) and frame.get("schema_version") == 1
            and frame.get("episode_id") == episode_id
            and frame.get("type") in ("snapshot", "update", "resync-required")
            and isinstance(frame.get("event_id"), str)
            and re.fullmatch(r"[A-Za-z0-9_-]{1,1024}", frame["event_id"])
            and isinstance(frame.get("deployment_id"), str)
            and re.fullmatch(r"[a-f0-9]{32}", frame["deployment_id"])
            and type(frame.get("sequence")) is int and frame["sequence"] >= 0
            and all(type(frame.get(key)) is bool for key in ("terminal", "waiting_for_operator", "requires_action"))
            and all(isinstance(frame.get(key), str) for key in ("phase", "stage", "next_action")))


def watch(opener, request, args):
    """Persistent observer; no admission, acceptance, cancellation or model calls."""
    retained = None
    if args.cursor_file is not None and (not args.cursor_file.is_absolute() or args.cursor_file.is_symlink() or not args.cursor_file.parent.is_dir()):
        raise ValueError("cursor file requires an existing operator-owned absolute parent")
    if args.cursor_file is not None and args.cursor_file.exists():
        if args.cursor_file.is_symlink() or args.cursor_file.stat().st_mode & 0o077:
            raise ValueError("cursor file must be private and not a symlink")
        retained = json.loads(args.cursor_file.read_text())
        if not valid_frame(retained, args.episode_id):
            raise ValueError("cursor belongs to another episode or is invalid")
    deadline = time.monotonic() + args.duration
    backoff = 1
    cursor_reset_used = False
    while time.monotonic() < deadline:
        if retained:
            request.add_header("Last-Event-ID", retained["event_id"])
        try:
            with opener.open(request, timeout=min(args.timeout, max(0.1, deadline - time.monotonic()))) as response:
                if not response.headers.get("Content-Type", "").startswith("text/event-stream"):
                    raise ValueError("server does not support the event stream; no polling fallback")
                data, event_id, event_type, frame_size = [], "", "", 0
                while time.monotonic() < deadline:
                    raw = response.readline(65537)
                    if not raw:
                        break
                    if len(raw) > 65536:
                        raise ValueError("event line exceeds metadata bound")
                    line = raw.decode().rstrip("\r\n")
                    if not line:
                        if event_type == "observation-error":
                            raise ValueError("episode observation unavailable")
                        if data:
                            frame = json.loads("\n".join(data))
                            if (not valid_frame(frame, args.episode_id)
                                    or frame["event_id"] != event_id or frame["type"] != event_type):
                                raise ValueError("uncorrelated or invalid progress frame")
                            if retained:
                                deployment_changed = frame["deployment_id"] != retained["deployment_id"]
                                if deployment_changed and frame["type"] not in ("snapshot", "resync-required"):
                                    raise ValueError("deployment changed without a fresh snapshot")
                                if not deployment_changed and frame["sequence"] < retained["sequence"]:
                                    raise ValueError("progress sequence regressed")
                            duplicate = retained and frame["event_id"] == retained["event_id"]
                            if not duplicate:
                                print(json.dumps(frame), flush=True)
                                # Output before receipt: a crash may replay an update but
                                # cannot mark an undelivered notification as consumed.
                                save_cursor(args.cursor_file, frame)
                                retained = frame
                                if frame["type"] in ("snapshot", "resync-required"):
                                    cursor_reset_used = False
                            backoff = 1
                            if frame.get("terminal") or frame.get("requires_action") and not args.follow:
                                return 0
                        data, event_id, event_type, frame_size = [], "", "", 0
                    elif line.startswith("data:"):
                        frame_size += len(raw)
                        if frame_size > 65536:
                            raise ValueError("event exceeds metadata bound")
                        data.append(line[5:].lstrip(" "))
                    elif line.startswith("id:"):
                        event_id = line[3:].strip()
                    elif line.startswith("event:"):
                        event_type = line[6:].strip()
        except urllib.error.HTTPError as error:
            # The Engine rejects cursors from an earlier deployment with 409.
            # Clear only the observer cursor and reconnect for a current snapshot;
            # never repeat the caller's episode submission.
            if error.code == 409 and retained is not None and not cursor_reset_used:
                retained = None
                cursor_reset_used = True
                clear_request_cursor(request)
                continue
            if error.code < 500:
                raise
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            pass  # Reconnect only the observer; never resubmit/cancel the episode.
        remaining = deadline - time.monotonic()
        if remaining > 0:
            time.sleep(min(backoff, remaining))
            backoff = min(backoff * 2, 30)
    print("observer timed out; episode execution was not cancelled", file=sys.stderr)
    return 3


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="literal loopback HTTP URL")
    parser.add_argument("--token-file", required=True, type=Path)
    parser.add_argument("--timeout", type=float, default=20)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("flows")
    commands.add_parser("capabilities")
    commands.add_parser("catalog")
    observe = commands.add_parser("watch")
    observe.add_argument("episode_id")
    observe.add_argument("--follow", action="store_true", help="remain quiet while operator acceptance is pending")
    observe.add_argument("--cursor-file", type=Path)
    observe.add_argument("--duration", type=float, default=1800)
    validate = commands.add_parser("validate")
    validate.add_argument("request_file", type=Path)
    submit = commands.add_parser("submit")
    submit.add_argument("request_file", type=Path)
    for command in ("status", "result", "cancel"):
        commands.add_parser(command).add_argument("episode_id")
    args = parser.parse_args()

    try:
        url = urllib.parse.urlsplit(args.url)
        if (url.scheme != "http" or not ipaddress.ip_address(url.hostname).is_loopback
                or url.username or url.password or not url.port
                or url.path not in ("", "/") or url.query or url.fragment):
            raise ValueError("URL must be http://literal-loopback:port")
        if not 0 < args.timeout <= 30:
            raise ValueError("timeout must be positive and at most 30 seconds")
        info = args.token_file.stat()
        if not args.token_file.is_absolute() or not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise ValueError("token file must be an absolute private regular file")
        token = args.token_file.read_text().strip()
        if len(token) < 32:
            raise ValueError("token is too short")

        body = None
        method = "GET"
        if args.command in ("submit", "validate"):
            body = args.request_file.read_bytes()
            if len(body) > 1 << 20:
                raise ValueError("request exceeds the API limit")
            json.loads(body)
            route, method = ("/episodes/validate" if args.command == "validate" else "/episodes"), "POST"
        elif args.command in ("flows", "capabilities", "catalog"):
            route = "/" + args.command
        else:
            route = "/episodes/" + urllib.parse.quote(args.episode_id, safe="")
            if args.command == "result":
                route += "/result"
            elif args.command == "cancel":
                route += "/cancel"
                method, body = "POST", b"{}"
            elif args.command == "watch":
                if not 0 < args.duration <= 86400:
                    raise ValueError("watch duration must be positive and at most one day")
                route += "/events"
        request = urllib.request.Request(args.url.rstrip("/") + route, data=body,
            method=method, headers={"Authorization": "Bearer " + token,
                                    "Content-Type": "application/json"})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
        if args.command == "watch":
            request.add_header("Accept", "text/event-stream")
            return watch(opener, request, args)
        # Submission is deliberately not retried here. The caller can reconcile
        # by its stable episode identity; this client is not another scheduler.
        with opener.open(request, timeout=args.timeout) as response:
            print(response.read().decode())
        return 0
    except urllib.error.HTTPError as error:
        print(error.read().decode())
        return 1
    except (OSError, ValueError, TypeError, urllib.error.URLError) as error:
        print(str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
