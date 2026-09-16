#!/usr/bin/env python3
"""Check finite consent during relay loss and no write replay after a lost reply."""
import argparse
import json
import shlex
import time
from pathlib import Path
from campaign import BASE, ENV, IPS, Session, initialize, request, trial
from deploy import Remote


def relay_loss(relay):
    def test(sessions, folder, report):
        citizen = sessions['citizen']
        assert report['outcome'] == 'punch_verified', report
        started = time.monotonic()
        initial = citizen.command({'op': 'status'})['direct']
        original_expiry = started + initial['remaining_ms'] / 1000
        relay.close()
        disconnected = {}
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            disconnected = {role: session.command({'op': 'status'}) for role, session in sessions.items()}
            if all(not status['relay_endpoint']['available'] for status in disconnected.values()):
                break
            time.sleep(0.05)
        assert all(not status['relay_endpoint']['available'] for status in disconnected.values()), disconnected
        during = request(citizen, 'SELECT count(*) FROM notes')
        assert during['result']['ok'] and during['direct']['active'], during
        remaining = during['direct']['remaining_ms']
        expected_remaining = (original_expiry - time.monotonic()) * 1000
        assert abs(remaining - expected_remaining) < 1000, (initial, during, expected_remaining)
        time.sleep(max(0, original_expiry - time.monotonic()) + 0.5)
        expired_status = citizen.command({'op': 'status'})
        assert not expired_status['direct']['active'], expired_status
        after = request(citizen, 'SELECT count(*) FROM notes')
        assert not after['result']['ok'], after
        return {'kind': 'relay_loss', 'during': during, 'after_expiry': after,
                'initial_remaining_ms': initial['remaining_ms'], 'disconnected': disconnected, 'elapsed_ms': round((time.monotonic() - started) * 1000)}
    return test


def reply_loss(remote):
    def test(sessions, folder, report):
        citizen = sessions['citizen']
        assert report['outcome'] == 'punch_verified', report
        before = citizen.command({'op': 'status'})
        assert before['direct']['active'] and before['direct']['selected'] == 'punch', before
        marker = 'arc-lab-interrupted-write'
        citizen.send({'op': 'request', 'body': json.dumps({'sql': 'INSERT INTO notes(marker) VALUES (?)', 'params': [marker]})})
        # The test provider signals only after the real SQLite transaction commits.
        remote.run('provider', f'timeout 10 bash -c "until test -f {folder}/receipt; do sleep 0.05; done"', timeout=12)
        for role in ('citizen', 'provider'):
            peer = 'provider' if role == 'citizen' else 'citizen'
            remote.run(role, f'sudo iptables -I ARC_LAB_FORWARD 1 -s {IPS[peer]} -j DROP; sudo iptables -I ARC_LAB_FORWARD 1 -d {IPS[peer]} -j DROP')
        remote.run('provider', 'touch ' + folder + '/receipt.release')
        result = citizen.receive(15)
        assert not result['result']['ok'] and result['result']['error'] == 'outcome_unknown', result
        time.sleep(0.5)
        query = ('import json,sqlite3; c=sqlite3.connect(' + repr(folder + '/main.db') + '); '
                 'print(json.dumps(c.execute("SELECT count(*) FROM notes WHERE marker=?", (' + repr(marker) + ',)).fetchone()))')
        count = json.loads(remote.run('provider', 'cd /opt/arc/providers/sqlite && env ' + ENV + ' /home/ubuntu/.local/bin/mise exec -- python -c ' + shlex.quote(query)))[0]
        receipts = int(remote.run('provider', 'wc -l < ' + folder + '/receipt').strip())
        assert count == 1 and receipts == 1, {'count': count, 'receipts': receipts}
        for role in ('citizen', 'provider'):
            (remote.output / (role + '-post-drop-network.txt')).write_text(remote.run(role, 'sudo /opt/arc/scripts/traversal_lab/network.sh evidence'))
            (remote.output / (role + '-post-drop-headers.txt')).write_text(remote.run(role, 'sudo cat ' + folder + '/headers.txt'))
        return {'kind': 'reply_loss_after_commit', 'before': before, 'result': result, 'durable_marker_count': count, 'provider_executions': receipts}
    return test


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--key', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    remote = Remote(json.loads(args.state.read_text()), args.key, args.output)
    key = initialize(remote, 'relay', BASE + '/relay')
    relay_config = {'host': IPS['relay'], 'port': 7331, 'public_key': key}
    remote.json_file('relay', BASE + '/relay.json', {'port': 7331, 'public_key': key})
    for index, name in enumerate(('relay_loss', 'reply_loss'), start=1):
        relay = Session(remote, 'relay', BASE + '/relay.json', name)
        try:
            hook = relay_loss(relay) if name == 'relay_loss' else reply_loss(remote)
            report = trial(remote, relay_config, 'stable', index, hook=hook, delayed=name == 'reply_loss')
            (remote.output / (name + '.json')).write_text(json.dumps(report, indent=2) + '\n')
            print(json.dumps(report['fault']), flush=True)
        finally:
            if relay.process.poll() is None:
                relay.close()


if __name__ == '__main__':
    main()
