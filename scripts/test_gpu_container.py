"""Regression-test the actual Dockerfile artifact loop without Docker or models.

Run: python3 scripts/test_gpu_container.py
"""
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest


class ModelArtifactGate(unittest.TestCase):
    def test_each_artifact_is_required(self):
        dockerfile = Path(__file__).resolve().parents[1] / "Dockerfile.migraphx"
        logical = dockerfile.read_text().replace("\\\n", " ")
        instruction = next(
            line.removeprefix("RUN ")
            for line in logical.splitlines()
            if line.startswith("RUN for d in /opt/attndb/models ")
        )
        # Exercise the real loop in its original &&-list context: a plain
        # 'set -e' fix would not reliably stop errors inside that context.
        loop, _ = instruction.split("&& groupadd", 1)
        artifacts = [
            Path(directory) / name
            for directory in (".", "single")
            for name in ("model.onnx", "model.onnx.data", "tokenizer.json")
        ]
        scenarios = [("complete", None)]
        scenarios += [(mode, path) for mode in ("missing", "empty") for path in artifacts]
        for mode, broken in scenarios:
            with self.subTest(mode=mode, artifact=broken), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp) / "models"
                for artifact in artifacts:
                    path = root / artifact
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text("fixture")
                if mode == "missing":
                    (root / broken).unlink()
                elif mode == "empty":
                    (root / broken).write_text("")
                command = loop.replace("/opt/attndb/models", shlex.quote(str(root)))
                command += " && printf downstream-reached"
                result = subprocess.run(
                    ["sh", "-c", command], text=True, capture_output=True, timeout=5
                )
                if mode == "complete":
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, "downstream-reached")
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("downstream-reached", result.stdout)
                    self.assertIn(str(root / broken), result.stderr)


if __name__ == "__main__":
    unittest.main()
