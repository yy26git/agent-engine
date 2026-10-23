import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
import urllib.request
from types import SimpleNamespace

import client


def progress_frame(*, event_id, deployment_id, sequence=0, kind="snapshot"):
    return {
        "schema_version": 1,
        "episode_id": "EP-TEST",
        "type": kind,
        "event_id": event_id,
        "deployment_id": deployment_id,
        "sequence": sequence,
        "terminal": True,
        "waiting_for_operator": False,
        "requires_action": False,
        "phase": "DONE",
        "stage": "COMPLETE",
        "next_action": "inspect_result",
    }


class Response:
    headers = {"Content-Type": "text/event-stream"}

    def __init__(self, frame):
        self.lines = iter((
            ("id: " + frame["event_id"] + "\n").encode(),
            ("event: " + frame["type"] + "\n").encode(),
            ("data: " + json.dumps(frame) + "\n").encode(),
            b"\n",
        ))

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def readline(self, _limit):
        return next(self.lines, b"")


class SequencedOpener:
    def __init__(self, responses):
        self.responses = iter(responses)
        self.cursor_headers = []

    def open(self, request, timeout):
        self.cursor_headers.append(request.get_header("Last-event-id"))
        response = next(self.responses)
        if isinstance(response, BaseException):
            raise response
        return response


class WatchRecoveryTests(unittest.TestCase):
    def args(self, cursor_file):
        return SimpleNamespace(
            cursor_file=cursor_file,
            episode_id="EP-TEST",
            duration=3,
            timeout=1,
            follow=False,
        )

    def test_stale_deployment_cursor_reconnects_with_snapshot_without_resubmitting(self):
        with tempfile.TemporaryDirectory() as directory:
            cursor = Path(directory) / "cursor.json"
            old = progress_frame(event_id="old-event", deployment_id="a" * 32, sequence=7)
            cursor.write_text(json.dumps(old))
            cursor.chmod(0o600)
            request = urllib.request.Request("http://127.0.0.1:8080/episodes/EP-TEST/events")
            snapshot = progress_frame(event_id="new-event", deployment_id="b" * 32, sequence=2)
            conflict = urllib.error.HTTPError(request.full_url, 409, "stale cursor", {}, io.BytesIO())
            opener = SequencedOpener((conflict, Response(snapshot)))

            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                result = client.watch(opener, request, self.args(cursor))

            self.assertEqual(result, 0)
            self.assertEqual(opener.cursor_headers[0], "old-event")
            self.assertIsNone(opener.cursor_headers[1])
            self.assertEqual(json.loads(output.getvalue()), snapshot)
            self.assertEqual(json.loads(cursor.read_text()), snapshot)

    def test_changed_deployment_is_accepted_only_with_fresh_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            cursor = Path(directory) / "cursor.json"
            old = progress_frame(event_id="old-event", deployment_id="a" * 32, sequence=7)
            cursor.write_text(json.dumps(old))
            cursor.chmod(0o600)
            request = urllib.request.Request("http://127.0.0.1:8080/episodes/EP-TEST/events")
            snapshot = progress_frame(event_id="new-event", deployment_id="b" * 32, sequence=2)
            opener = SequencedOpener((Response(snapshot),))

            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                result = client.watch(opener, request, self.args(cursor))

            self.assertEqual(result, 0)
            self.assertEqual(json.loads(output.getvalue()), snapshot)
            self.assertEqual(json.loads(cursor.read_text()), snapshot)


if __name__ == "__main__":
    unittest.main()
