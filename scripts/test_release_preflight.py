import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('release_preflight', Path(__file__).with_name('release-preflight.py'))
preflight = importlib.util.module_from_spec(spec)
spec.loader.exec_module(preflight)


class ReleasePreflightTests(unittest.TestCase):
    def test_only_exact_successful_default_branch_preflight_is_reused(self):
        run = dict(id=42, head_sha='abc', head_branch='main', path='.github/workflows/release.yml',
                   event='workflow_dispatch', display_title='Release v0.10.0', status='completed', conclusion='success')
        self.assertEqual(preflight.select_run([run], 'abc', 'main', 'v0.10.0'), '42')
        for key, value in dict(head_sha='older', head_branch='feature', path='.github/workflows/full-validation.yml',
                               event='pull_request', display_title='Release v0.11.0', status='in_progress', conclusion='failure').items():
            with self.subTest(key=key):
                self.assertEqual(preflight.select_run([dict(run, **{key: value})], 'abc', 'main', 'v0.10.0'), '')
        self.assertEqual(preflight.select_run([], 'abc', 'main', 'v0.10.0'), '')
        self.assertEqual(preflight.select_run([dict(run, conclusion='failure'), run], 'abc', 'main', 'v0.10.0'), '42')
