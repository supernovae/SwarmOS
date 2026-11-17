#!/usr/bin/env python3
"""Select only a successful default-branch release preflight at the exact SHA."""
import json
from pathlib import Path
import sys


def select_run(runs, commit, branch, tag):
    for run in runs:
        if (run['head_sha'] == commit and run['head_branch'] == branch
                and run['path'] == '.github/workflows/release.yml'
                and run['event'] == 'workflow_dispatch'
                and run['display_title'] == f'Release {tag}'
                and run['status'] == 'completed' and run['conclusion'] == 'success'):
            return str(run['id'])
    return ''


if __name__ == '__main__':
    print(select_run(json.loads(Path(sys.argv[1]).read_text())['workflow_runs'], sys.argv[2], sys.argv[3], sys.argv[4]))
