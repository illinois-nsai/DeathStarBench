"""Offline checks for bounded graph initialization."""
import asyncio
from collections import Counter
from contextlib import redirect_stdout
import importlib.util
import io
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "init_social_graph", Path(__file__).resolve().parents[1] / "scripts/init_social_graph.py")
init = importlib.util.module_from_spec(spec)
spec.loader.exec_module(init)


class InitializationTests(unittest.IsolatedAsyncioTestCase):
    async def test_slow_request_does_not_stall_other_workers(self):
        release, progressed = asyncio.Event(), asyncio.Event()
        seen = []
        active = maximum = 0

        async def request(item):
            nonlocal active, maximum
            active += 1
            maximum = max(maximum, active)
            try:
                if item == 0:
                    await release.wait()
                else:
                    await asyncio.sleep(0)
                seen.append(item)
                if len(seen) == 20:
                    progressed.set()
                return "Success!"
            finally:
                active -= 1

        task = asyncio.create_task(init.run_bounded(range(40), request, 4))
        try:
            await asyncio.wait_for(progressed.wait(), 2)
            self.assertNotIn(0, seen)
        finally:
            release.set()
            results = await task
        self.assertEqual(sorted(seen), list(range(40)))
        self.assertEqual(results, ["Success!"] * 40)
        self.assertLessEqual(maximum, 4)

    async def test_existing_user_ids_and_both_follow_directions_are_preserved(self):
        users, follows = [], []
        async def register(session, addr, user):
            users.append(user)
            await asyncio.sleep(0)
            return "Success!"
        async def follow(session, addr, a, b):
            follows.append((a, b))
            await asyncio.sleep(0)
            return "Success!"
        with patch.object(init, "upload_register", register), patch.object(init, "upload_follow", follow), \
                redirect_stdout(io.StringIO()):
            await init.register("unused", 32, limit=3)
            await init.follow("unused", [("0", "1"), ("1", "2")], limit=3)
        self.assertEqual(Counter(users), Counter(map(str, range(32))))
        self.assertEqual(Counter(follows), Counter([("0", "1"), ("1", "0"), ("1", "2"), ("2", "1")]))

    async def test_failed_request_cancels_workers_without_retry(self):
        seen = []
        async def request(item):
            seen.append(item)
            if item == 0:
                raise RuntimeError("request failed")
            await asyncio.sleep(60)
        with self.assertRaisesRegex(RuntimeError, "request failed"):
            await init.run_bounded(range(100), request, 4)
        self.assertLessEqual(len(seen), 4)
        self.assertEqual(seen.count(0), 1)

    async def test_registration_errors_return_failure(self):
        async def register(session, addr, user):
            return "User registration failure: transport closed" if user == "0" else "Success!"
        with patch.object(init, "upload_register", register), redirect_stdout(io.StringIO()):
            self.assertFalse(await init.register("unused", 3, limit=2))
