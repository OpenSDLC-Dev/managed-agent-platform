#!/usr/bin/env python3
"""Verify Helm chart defaults enable executor.gateImage with a real release ref."""
import re
import unittest
from pathlib import Path

CHART = Path(__file__).resolve().parents[1]


def extract_top_level_block(text: str, key: str) -> str:
    match = re.search(rf"(?ms)^{re.escape(key)}:\n(.*?)(?=^\S|\Z)", text)
    if not match:
        raise AssertionError(f"missing top-level {key!r} block")
    return match.group(1)


def extract_yaml_scalar(block: str, key: str, indent: int = 2) -> str:
    prefix = " " * indent
    match = re.search(rf'(?m)^{prefix}{re.escape(key)}:\s*("([^"]*)"|\'([^\']*)\'|([^\s#]+))', block)
    if not match:
        raise AssertionError(f"missing key {key!r} at indent {indent}")
    for group in (match.group(2), match.group(3), match.group(4)):
        if group is not None:
            return group
    return ""


class GateDefaultTest(unittest.TestCase):
    def test_default_values_enable_shippable_gate_image(self):
        chart_text = (CHART / "Chart.yaml").read_text()
        values_text = (CHART / "values.yaml").read_text()

        app_match = re.search(r'(?m)^appVersion:\s*"([^"]+)"', chart_text)
        self.assertIsNotNone(app_match, "Chart.yaml must define quoted appVersion")
        app_version = app_match.group(1).strip()

        image_block = extract_top_level_block(values_text, "image")
        registry = extract_yaml_scalar(image_block, "registry")
        repository = extract_yaml_scalar(image_block, "repository")
        expected = f"{registry}/{repository}/gate:{app_version}"

        executor_block = extract_top_level_block(values_text, "executor")
        gate_image = extract_yaml_scalar(executor_block, "gateImage")
        self.assertTrue(gate_image, "executor.gateImage must be non-empty by default")
        self.assertEqual(
            gate_image,
            expected,
            f"executor.gateImage ({gate_image!r}) must match the chart's published gate image ({expected!r})",
        )

    def test_executor_deployment_wires_gate_image_and_controlplane_url(self):
        template = (CHART / "templates/executor-deployment.yaml").read_text()
        self.assertIn("{{- with .Values.executor.gateImage }}", template)
        self.assertRegex(template, r"- name: EXECUTOR_GATE_IMAGE\n\s+value: \{\{ \. \| quote \}\}")
        self.assertIn("- name: CONTROLPLANE_URL", template)


if __name__ == "__main__":
    unittest.main(verbosity=2)
