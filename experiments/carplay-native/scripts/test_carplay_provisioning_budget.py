"""以确定性时间验证首次登记预算、取消及旧请求不能复活任务。"""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import carplay_provisioning as module


class BudgetTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory()
        self.addCleanup(self.root.cleanup)
        self.wall, self.mono = 1000.0, 10.0
        self.clock = patch.object(module.time, "time", side_effect=lambda: self.wall)
        self.monotonic = patch.object(module.time, "monotonic", side_effect=lambda: self.mono)
        self.clock.start()
        self.monotonic.start()
        self.addCleanup(self.clock.stop)
        self.addCleanup(self.monotonic.stop)
        self.flow = module.Provisioning(self.root.name)
        self.events = []
        self.pause = dict(request_id="a" * 32, phone_id="02:11:22:33:44:55", action="pause",
                          expires_at_ms=1060000, total_expires_at_ms=1180000)

    def switch(self, mode, phone, seconds):
        self.events.append((mode, seconds))
        return True

    def advance(self, seconds):
        self.wall += seconds
        self.mono += seconds

    def enroll(self):
        return dict(self.pause, action="enroll", expires_at_ms=min(int((self.wall + 120) * 1000), 1180000))

    def test_setup_does_not_consume_video_budget_and_cannot_renew(self):
        self.flow.apply(self.pause, self.switch)
        self.advance(50)
        request = self.enroll()
        self.flow.apply(request, self.switch)
        deadline = self.flow.deadline
        self.assertEqual(self.events, [("paused", 0), ("enroll", 120)])
        self.advance(10)
        self.flow.apply(self.enroll(), self.switch)
        self.flow.apply(self.pause, self.switch)
        self.assertEqual(self.flow.deadline, deadline)
        self.assertEqual(len(self.events), 2)
        self.advance(111)
        self.flow.poll(self.switch)
        self.assertEqual(self.flow.state["state"], "expired")
        self.flow.apply(request, self.switch)
        self.assertIsNone(self.flow.active)
        self.assertEqual(len(self.events), 3)

    def test_total_deadline_is_immutable_and_monotonic(self):
        self.flow.apply(self.pause, self.switch)
        self.advance(59)
        self.flow.apply(dict(self.enroll(), total_expires_at_ms=1300000), self.switch)
        self.assertEqual(self.flow.state["state"], "paused")
        self.flow.apply(self.enroll(), self.switch)
        self.wall -= 300
        self.mono += 121
        self.flow.poll(self.switch)
        self.assertEqual(self.flow.state["state"], "expired")

    def test_slow_pause_does_not_extend_absolute_budget(self):
        def slow_switch(mode, phone, seconds):
            if mode == "paused":
                self.advance(20)
            return self.switch(mode, phone, seconds)
        self.flow.apply(self.pause, slow_switch)
        self.assertEqual(self.flow.deadline, 70.0)
        self.assertEqual(self.flow.total_deadline, 190.0)
        self.advance(40)
        self.flow.apply(self.enroll(), self.switch)
        self.assertEqual(self.flow.state["state"], "expired")

    def test_pause_callback_crosses_deadline_without_reporting_ready(self):
        def slow_switch(mode, phone, seconds):
            if mode == "paused":
                self.advance(61)
            return self.switch(mode, phone, seconds)
        self.flow.apply(self.pause, slow_switch)
        self.assertEqual(self.flow.state["state"], "expired")
        self.assertIsNone(self.flow.active)
        self.assertEqual(self.events, [("paused", 0), ("resume", 0)])
        self.assertEqual(self.flow.state["request_id"], self.pause["request_id"])

    def test_enroll_request_processing_crosses_deadline_without_starting(self):
        self.flow.apply(self.pause, self.switch)
        self.advance(59)
        request = self.enroll()
        reads = 0
        def advancing_clock():
            nonlocal reads
            reads += 1
            if reads == 4:
                self.advance(2)
            return self.mono
        with patch.object(module.time, "monotonic", side_effect=advancing_clock):
            self.flow.apply(request, self.switch)
        self.assertEqual(self.flow.state["state"], "expired")
        self.assertFalse(any(mode == "enroll" for mode, _ in self.events))

    def test_expired_setup_or_cancel_cannot_start_video(self):
        for cancel in (False, True):
            self.wall, self.mono = 1000.0, 10.0
            flow = module.Provisioning(self.root.name)
            flow.apply(self.pause, self.switch)
            if cancel:
                flow.apply(dict(self.pause, action="cancel"), self.switch)
            else:
                self.advance(60)
            flow.apply(self.enroll(), self.switch)
            self.assertIsNone(flow.active)
        self.assertFalse(any(mode == "enroll" for mode, _ in self.events))

    def test_wrong_phone_and_extra_budget_are_rejected(self):
        self.flow.apply(self.pause, self.switch)
        self.flow.apply(dict(self.enroll(), phone_id="02:11:22:33:44:66"), self.switch)
        self.flow.apply(dict(self.enroll(), expires_at_ms=1180000), self.switch)
        self.assertEqual(self.flow.state["state"], "paused")
        self.assertEqual(len(self.events), 1)

    @unittest.skipUnless(os.name == "posix", "私有运行文件重放验证需要 Linux")
    def test_restart_never_replays_previous_phased_request(self):
        request = Path(self.root.name) / "provisioning-request.json"
        request.write_text(json.dumps(self.pause), encoding="utf-8")
        request.chmod(0o600)
        flow = module.Provisioning(self.root.name)
        flow.poll(self.switch)
        flow.apply(self.pause, self.switch)
        flow.apply(self.enroll(), self.switch)
        self.assertEqual(flow.state["state"], "canceled")
        self.assertIsNone(flow.active)
        self.assertEqual(self.events, [])

    def test_manual_window_and_media_gate_unchanged(self):
        request = {k: v for k, v in self.pause.items() if k != "total_expires_at_ms"}
        self.flow.apply(request, self.switch)
        self.flow.apply(dict(request, action="enroll"), self.switch)
        self.assertEqual(self.events, [("paused", 0), ("enroll", 60)])
        receiver = dict(stage="phone_media_success", session_verified=True,
                        media=dict(frames=1, source_connected=True, codec_ready=True))
        self.assertFalse(self.flow.observe(dict(receiver, session_verified=False), request["phone_id"]))
        self.assertFalse(self.flow.observe(receiver, "02:11:22:33:44:66"))
        self.advance(60)
        self.assertFalse(self.flow.observe(receiver, request["phone_id"]))

    def test_receiver_start_rechecks_budget_after_network_preparation(self):
        self.flow.apply(self.pause, self.switch)
        self.advance(20)
        self.flow.apply(self.enroll(), self.switch)
        self.assertEqual(self.flow.enrollment_seconds(), 120)
        self.advance(119.5)
        self.assertEqual(self.flow.enrollment_seconds(), 1)
        self.advance(0.5)
        with self.assertRaises(TimeoutError):
            self.flow.enrollment_seconds()
        self.flow.fail()
        self.assertEqual(self.flow.state["state"], "expired")


if __name__ == "__main__":
    unittest.main()
