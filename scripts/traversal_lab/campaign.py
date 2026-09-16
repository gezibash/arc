#!/usr/bin/env python3
"""Run synthetic SQLite requests through real ARC processes in the disposable lab."""
import argparse
import concurrent.futures
import json
import queue
import shlex
import subprocess
import threading
import time
from pathlib import Path
from deploy import Remote

BASE = '/opt/arc/lab/' + time.strftime('%Y%m%dT%H%M%S')
RUNNER = 'scripts/traversal_lab/runner.exs'
ENV = 'PATH=/home/ubuntu/.local/bin:/usr/local/bin:/usr/bin:/bin MIX_ENV=prod'
IPS = {'relay': '10.88.0.10', 'citizen': '10.88.0.11', 'provider': '10.88.0.12'}


def runner_command(role, args, namespace=False, extra_env=''):
    command = f'cd /opt/arc && umask 077 && '
    if namespace:
        command += 'sudo ip netns exec arc-endpoint sudo -u ubuntu -H '
    command += f'env {ENV} {extra_env} /home/ubuntu/.local/bin/mise exec -- mix run --no-compile --no-start {RUNNER} -- '
    return command + shlex.join(args)


class Session:
    def __init__(self, remote, role, config, label, extra_env=''):
        self.role = role
        self.events = queue.Queue()
        self.log = (remote.output / (label + '-' + role + '.jsonl')).open('w')
        self.process = subprocess.Popen(remote.command(role, runner_command(role, [role, config], role != 'relay', extra_env)),
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()
        self.ready = self.receive(30)
        if self.ready.get('event') != 'ready':
            raise RuntimeError(f'{role} failed to become ready: {self.ready}')

    def read(self):
        for line in self.process.stdout:
            if line.startswith('ARC_LAB '):
                value = json.loads(line[8:])
                self.log.write(json.dumps(value) + '\n')
                self.log.flush()
                self.events.put(value)
        self.events.put({'event': 'eof'})

    def send(self, value):
        self.process.stdin.write(json.dumps(value) + '\n')
        self.process.stdin.flush()

    def receive(self, timeout=20):
        return self.events.get(timeout=timeout)

    def command(self, value, timeout=20):
        self.send(value)
        return self.receive(timeout)

    def close(self):
        if self.process.poll() is None:
            self.send({'op': 'shutdown'})
            self.process.stdin.close()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=5)
        self.reader.join(timeout=5)
        if self.reader.is_alive():
            raise RuntimeError('SSH reader did not finish during cleanup')
        self.log.close()


def initialize(remote, role, state):
    text = remote.run(role, runner_command(role, ['initialize', state]))
    result = [json.loads(line[8:]) for line in text.splitlines() if line.startswith('ARC_LAB ')][-1]
    if result.get('event') != 'initialized':
        raise RuntimeError(f'Failed to initialize {role}: {result}')
    return result['public_key']


def policy(peer, dial, enabled=True, listener=None, lease=30000):
    if not enabled:
        return {'version': 1, 'rules': []}
    rule = {'peer': peer, 'capability': 'primary', 'scheme': 'sqlite', 'path': '/main',
            'lease_ms': lease, 'dial': [dial], 'hole_punch': listener is None}
    if listener:
        rule['hole_punch'] = False
        if listener != 'dial':
            rule['listen'] = {'bind': '10.200.0.2', 'address': listener, 'port': 4444}
    return {'version': 1, 'rules': [rule]}


def request(session, sql, params=None):
    body = {'sql': sql}
    if params is not None:
        body['params'] = params
    return session.command({'op': 'request', 'body': json.dumps(body)})


def trial(remote, relay_config, name, index, hook=None, delayed=False):
    label = f'{name}-{index}'
    folder = f'{BASE}/{label}'
    profiles = {role: ('stable' if name in ('no_permission', 'one_owner', 'denied_address', 'provider_listener', 'citizen_listener') else name)
                for role in ('citizen', 'provider')}
    if name in ('provider_listener', 'citizen_listener'):
        profiles[name.split('_')[0]] = 'listener'
    for role in ('citizen', 'provider'):
        remote.run(role, f'sudo /opt/arc/scripts/traversal_lab/network.sh profile {profiles[role]}')
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        futures = {role: pool.submit(initialize, remote, role, folder) for role in ('citizen', 'provider')}
        keys = {role: future.result() for role, future in futures.items()}
    for role in ('citizen', 'provider'):
        peer = 'provider' if role == 'citizen' else 'citizen'
        enabled = name != 'no_permission' and not (name == 'one_owner' and role == 'provider')
        dial = '10.88.0.99' if name == 'denied_address' and role == 'citizen' else IPS[peer]
        listener = None
        if name.endswith('_listener'):
            listener = IPS[role] if name.startswith(role) else 'dial'
        rules = policy(keys[peer], dial, enabled, listener)
        remote.json_file(role, folder + '/policy.json', rules)
        config = {'state_root': folder, 'relay': relay_config, 'direct_policy': folder + '/policy.json'}
        if role == 'provider':
            runtime = '/opt/arc/scripts/traversal_lab/delayed_sqlite.sh' if delayed else '/opt/arc/providers/sqlite/run.sh'
            config['serve'] = 'exec://' + runtime + '?manifest=/opt/arc/providers/sqlite/manifest.json'
            remote.json_file(role, folder + '/sqlite.json', {'databases': {'main': {'path': folder + '/main.db', 'grants': {keys['citizen']: 'write'}}}})
        else:
            config.update(address='sqlite+arc://' + keys['provider'] + '/main', request_timeout_ms=10000)
        remote.json_file(role, folder + '/config.json', config)
        # Header-only text: no -A/-X or raw packet capture. Both sides of NAT visible.
        remote.run(role, f"sudo sh -c 'nohup timeout 60 tcpdump -i any -nn -tttt -l \"tcp and host {IPS[peer]}\" > {folder}/headers.txt 2>&1 < /dev/null &'")
    sessions = {}
    try:
        sessions['provider'] = Session(remote, 'provider', folder + '/config.json', label, 'SQLITE_CONFIG=' + folder + '/sqlite.json ARC_LAB_RECEIPT=' + folder + '/receipt')
        sessions['citizen'] = Session(remote, 'citizen', folder + '/config.json', label)
        endpoints = {r: s.command({'op': 'status'}) for r, s in sessions.items()}
        probes = {}
        for target in ('citizen', 'provider'):
            if profiles[target] == 'listener':
                continue
            peer = 'provider' if target == 'citizen' else 'citizen'
            ep = endpoints[target]['relay_endpoint']['observed']
            command = f"if timeout 2 bash -c 'exec 3<>/dev/tcp/{ep['host']}/{ep['port']}'; then echo unexpected_inbound; else echo inbound_blocked; fi"
            probes[target] = remote.run(peer, command).strip()
            assert probes[target] == 'inbound_blocked', probes
        results = []
        results.append(request(sessions['citizen'], 'CREATE TABLE notes (marker TEXT)'))
        results.append(request(sessions['citizen'], 'INSERT INTO notes(marker) VALUES (?)', [label]))
        results.append(request(sessions['citizen'], 'SELECT marker FROM notes'))
        sql_ok = all(r.get('result', {}).get('ok') for r in results)
        sql_ok = sql_ok and results[-1]['result'].get('body_json', {}).get('results', [{}])[0].get('rows') == [[label]]
        direct = results[-1].get('direct', {})
        # Read database independently on its host after the ARC write/read exercise.
        read_code = 'import json,sqlite3; c=sqlite3.connect(' + repr(folder + '/main.db') + '); print(json.dumps(c.execute("SELECT marker,count(*) FROM notes GROUP BY marker").fetchall()))'
        durable = json.loads(remote.run('provider', 'cd /opt/arc/providers/sqlite && env ' + ENV + ' /home/ubuntu/.local/bin/mise exec -- python -c ' + shlex.quote(read_code)))
        report = {'profile': name, 'trial': index, 'sql_ok': sql_ok, 'direct': direct, 'durable': durable,
                  'elapsed_ms': [r.get('elapsed_ms') for r in results], 'endpoints': endpoints, 'inbound_probes': probes, 'results': results}
        mapping_checks = {}
        for role in sessions:
            evidence = remote.run(role, 'sudo /opt/arc/scripts/traversal_lab/network.sh evidence')
            (remote.output / f'{label}-{role}-network.txt').write_text(evidence)
            own = endpoints[role]['relay_endpoint']
            peer = 'provider' if role == 'citizen' else 'citizen'
            other = endpoints[peer]['relay_endpoint']['observed']
            local = own['local']
            observed = own['observed']
            relay_mapping = any(
                f"src=10.200.0.2 dst={IPS['relay']} sport={local['port']} dport=7331" in line and
                f"src={IPS['relay']} dst={observed['host']} sport=7331 dport={observed['port']}" in line
                for line in evidence.splitlines())
            peer_mapping = any(
                f"src=10.200.0.2 dst={other['host']} sport={local['port']} dport={other['port']}" in line and
                f"src={other['host']} dst={observed['host']} sport={other['port']} dport={observed['port']}" in line
                for line in evidence.splitlines())
            mapping_checks[role] = {'relay_mapping': relay_mapping, 'peer_mapping': peer_mapping}
            headers = remote.run(role, 'sudo cat ' + folder + '/headers.txt')
            (remote.output / f'{label}-{role}-headers.txt').write_text(headers)
        report['mappings'] = mapping_checks
        if direct.get('active') and direct.get('selected') == 'punch':
            assert all(check['relay_mapping'] and check['peer_mapping'] for check in mapping_checks.values()), report
            report['outcome'] = 'punch_verified'
        elif direct.get('active'):
            report['outcome'] = 'listener_verified'
        elif name in ('stable', 'impaired'):
            report['outcome'] = 'punch_failed_relay_worked'
        else:
            report['outcome'] = 'relay_fallback_verified'
        assert sql_ok and durable == [[label, 1]], report
        if name in ('no_permission', 'one_owner', 'denied_address', 'changed', 'blocked'):
            assert all(not item.get('direct', {}).get('active') for item in results), report
        if name in ('provider_listener', 'citizen_listener'):
            expected = 'provider' if name == 'provider_listener' else 'caller'
            assert direct.get('active') and direct.get('selected') == expected, report
        if hook is not None:
            report['fault'] = hook(sessions, folder, report)
        return report
    finally:
        for session in sessions.values():
            session.close()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--state', type=Path, required=True)
    p.add_argument('--key', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    p.add_argument('--profiles', default='stable,no_permission,one_owner,denied_address,changed,blocked,provider_listener,citizen_listener,impaired')
    p.add_argument('--repeat', type=int, default=3)
    p.add_argument('--setup', action='store_true')
    args = p.parse_args()
    remote = Remote(json.loads(args.state.read_text()), args.key, args.output)
    if args.setup:
        for role in ('citizen', 'provider'):
            peer = 'provider' if role == 'citizen' else 'citizen'
            remote.run(role, f'sudo /opt/arc/scripts/traversal_lab/network.sh setup {IPS[role]} {IPS[peer]} {IPS["relay"]}')
    relay_key = initialize(remote, 'relay', BASE + '/relay')
    relay_config = {'host': IPS['relay'], 'port': 7331, 'public_key': relay_key}
    remote.json_file('relay', BASE + '/relay.json', {'port': 7331, 'public_key': relay_key})
    relay = Session(remote, 'relay', BASE + '/relay.json', 'campaign')
    try:
        with (remote.output / 'results.jsonl').open('a') as output:
            for name in args.profiles.split(','):
                for i in range(1, args.repeat + 1):
                    report = trial(remote, relay_config, name, i)
                    output.write(json.dumps(report) + '\n')
                    output.flush()
                    print(json.dumps({k: report[k] for k in ('profile', 'trial', 'outcome', 'sql_ok', 'direct', 'elapsed_ms')}), flush=True)
    finally:
        relay.close()


if __name__ == '__main__':
    main()
