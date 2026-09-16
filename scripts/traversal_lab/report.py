#!/usr/bin/env python3
"""Summarize completed experiment records without converting fallback into punch success."""
import argparse
import hashlib
import json
import statistics
from collections import Counter, defaultdict
from pathlib import Path


def summarize(root):
    groups = defaultdict(list)
    evidence = {}
    for path in sorted(root.rglob('results.jsonl')):
        evidence[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
        for line in path.read_text().splitlines():
            row = json.loads(line)
            groups[row['profile']].append(row)
    profiles = {}
    for name, rows in sorted(groups.items()):
        first_requests = [r['elapsed_ms'][0] for r in rows]
        profiles[name] = {
            'trials': len(rows), 'outcomes': dict(Counter(r['outcome'] for r in rows)),
            'sql_requests': sum(len(r['results']) for r in rows),
            'all_sql_correct': all(r['sql_ok'] for r in rows),
            'all_durable_markers_once': all(r['durable'] == [[f"{r['profile']}-{r['trial']}", 1]] for r in rows),
            'first_request_ms': {'min': min(first_requests), 'median': statistics.median(first_requests), 'max': max(first_requests)},
        }
    faults = {}
    for name in ('relay_loss', 'reply_loss'):
        for path in sorted(root.rglob(name + '.json')):
            data = json.loads(path.read_text())
            faults[name] = data['fault']
            evidence[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return {'profiles': profiles, 'faults': faults, 'evidence_sha256': evidence,
            'scope': 'Separate EC2 endpoint hosts behind controlled Linux NAT; not universal consumer-router verification.'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('evidence', type=Path)
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    value = summarize(args.evidence)
    text = json.dumps(value, indent=2) + '\n'
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(text)
    print(text, end='')


if __name__ == '__main__':
    main()
