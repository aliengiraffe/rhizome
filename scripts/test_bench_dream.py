#!/usr/bin/env python3
"""Offline regression checks for benchmark orchestration and response parsing."""

import contextlib
import io
import json
import pathlib
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import bench_dream as bench


class BenchmarkTests(unittest.TestCase):
    def test_judge_files_are_separate_and_model_scoped(self):
        self.assertEqual(bench.grade_path("claude").name, "grades.jsonl")
        codex = bench.grade_path("codex")
        self.assertNotEqual(codex, bench.grade_path("claude"))
        with patch.object(bench, "CODEX_MODEL", "another-model"):
            self.assertNotEqual(codex, bench.grade_path("codex"))

    def test_repeated_judgments_never_share_a_batch(self):
        rows = [{"suite": "recall", "qid": "q", "agent": "codex",
                 "arm": "none", "rep": rep} for rep in range(6)]
        batches = []

        def grade(jobs, questions):
            batches.append(jobs)
            return []

        with patch.object(bench, "load_questions", return_value=[{"id": "q"}]), \
                patch.object(bench, "read_jsonl", side_effect=lambda p: rows if p.name == "recall.jsonl" else []), \
                patch.object(bench, "grade_codex_batch", side_effect=grade), \
                contextlib.redirect_stderr(io.StringIO()):
            bench.cmd_grade(SimpleNamespace(suite="recall", judge_agent="codex", batch_size=4,
                                            judge_repeats=2, jobs=1))
        self.assertEqual(len(batches), 4)
        self.assertEqual(sum(map(len, batches)), 12)
        self.assertTrue(all(len({rep for _, rep in batch}) == 1 for batch in batches))

    def test_codex_judge_validates_all_cases_before_accepting_batch(self):
        rows = [{"suite": "recall", "qid": "q", "agent": "codex",
                 "arm": arm, "rep": 0, "answer": "answer"} for arm in ("none", "oracle")]
        questions = {"q": {"question": "question", "facts": ["fact"]}}
        valid = [{"id": i, "facts": [bool(i)], "contradiction": False, "abstained": False}
                 for i in range(2)]
        for grades, tool_calls, succeeds in ((valid, 0, True), (valid[:1], 0, False),
                                             ([valid[0], valid[0]], 0, False), (valid, 1, False),
                                             ([dict(valid[0], facts=[]), valid[1]], 0, False)):
            with self.subTest(grades=grades, tool_calls=tool_calls), patch.object(
                    bench, "run_codex", return_value={"answer": json.dumps({"grades": grades}),
                                                     "tool_calls": tool_calls, "error": None}):
                if succeeds:
                    got = bench.grade_codex_batch([(r, 1) for r in rows], questions)
                    self.assertEqual([g["labels"] for g in got], [[False], [True]])
                    self.assertTrue(all(g["judge_rep"] == 1 for g in got))
                else:
                    with self.assertRaises((ValueError, RuntimeError)):
                        bench.grade_codex_batch([(r, 1) for r in rows], questions)

    def test_codex_checks_inputs_not_returned_text(self):
        for command, expected in (("cat README.md", False),
                                  ("cat ~/.claude/projects/session.jsonl", True)):
            with self.subTest(command=command):
                events = [
                    {"type": "item.completed", "item": {
                        "type": "command_execution", "command": command,
                        "aggregated_output": "Documentation mentions ~/.claude/projects"}},
                    {"type": "turn.completed", "usage": {}},
                ]
                result = subprocess.CompletedProcess([], 0, stdout="\n".join(map(json.dumps, events)))
                with patch.object(bench.subprocess, "run", return_value=result):
                    row = bench.run_codex("question", tempfile.gettempdir())
                self.assertEqual(row["leak"], expected)
                self.assertEqual(row["tool_inputs"][0]["input"], command)
                self.assertEqual(row["tool_calls"], 1)

    def test_claude_error_with_success_subtype_is_rejected(self):
        response = {"type": "result", "subtype": "success", "is_error": True,
                    "result": "Usage limit"}
        result = subprocess.CompletedProcess([], 0, stdout=json.dumps(response))
        with patch.object(bench.subprocess, "run", return_value=result):
            self.assertEqual(bench.run_claude("q", tempfile.gettempdir(), [])["error"], "agent_error")
            with self.assertRaises(RuntimeError):
                bench.call_claude_json("q", {}, "test-model")

    def test_open_checkout_created_before_workers(self):
        ready = []

        def run(job):
            self.assertEqual(ready, [True])
            return {"agent": "codex", "arm": job["arm"], "qid": "q",
                    "wall_s": 0, "error": None, "leak": False}

        args = SimpleNamespace(questions="", limit=0, suite="open", repeats=1,
                               agents="codex", arms="none,tools", jobs=2)
        with patch.object(bench, "load_questions", return_value=[{"id": "q"}]), \
                patch.object(bench, "read_jsonl", return_value=[]), \
                patch.object(bench, "worktree", side_effect=lambda: ready.append(True)), \
                patch.object(bench, "run_one", side_effect=run), \
                contextlib.redirect_stderr(io.StringIO()):
            bench.cmd_run(args)

    def test_grading_stops_on_unavailable_judge(self):
        rows = [{"suite": "recall", "qid": "q", "agent": "codex",
                 "arm": "none", "rep": rep} for rep in range(4)]
        with patch.object(bench, "load_questions", return_value=[{"id": "q"}]), \
                patch.object(bench, "read_jsonl", side_effect=lambda p: [] if p.name == "grades.jsonl" else rows), \
                patch.object(bench, "grade_row", side_effect=RuntimeError("unavailable")) as grade, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as raised:
                bench.cmd_grade(SimpleNamespace(suite="recall", judge_repeats=2, jobs=1))
        self.assertEqual(raised.exception.code, 1)
        self.assertEqual(grade.call_count, 1)

    def test_dream_resume_preserves_metadata_and_reports_failure(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(bench, "BENCH", pathlib.Path(tmp)):
            (bench.BENCH / "sample.toml").write_text('[dream]\nenabled=true\n')
            path = bench.BENCH / "memory/sample.json"
            bench.write_private(path, json.dumps({"variant": "sample", "settings": {},
                                                 "runs": [{"pass": 0, "rc": 0}]}))
            results = [subprocess.CompletedProcess([], 1, "", "dream failed"),
                       subprocess.CompletedProcess([], 0, "saved memory", "")]
            with patch.object(bench, "frozen_catalog", return_value={}), \
                    patch.object(bench.subprocess, "run", side_effect=results), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as raised:
                    bench.cmd_dream(SimpleNamespace(variant="sample", set=[], resume=True,
                                                   instructions=None, passes=2))
            self.assertEqual(raised.exception.code, 1)
            saved = json.loads(path.read_text())
            self.assertEqual([r["pass"] for r in saved["runs"]], [0, 1])
            self.assertEqual([r["rc"] for r in saved["runs"]], [0, 1])
            self.assertEqual((bench.BENCH / "memory/sample.md").read_text(), "saved memory")


if __name__ == "__main__":
    unittest.main()
