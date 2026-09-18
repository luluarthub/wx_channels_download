"""Exercise a built Windows binary with isolated state and real HTTP downloads.

No installed application, system proxy or user database is changed.
"""
import argparse
import hashlib
import http.client
import http.server
import json
import os
import pathlib
import re
import socket
import sqlite3
import subprocess
import threading
import time
import urllib.request
import urllib.parse


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def wait_for(check, label, timeout=35):
    end = time.monotonic() + timeout
    last = None
    while time.monotonic() < end:
        try:
            value = check()
            if value:
                return value
        except Exception as exc:
            last = exc
        time.sleep(0.15)
    raise AssertionError(f'{label} timed out; last error: {last}')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--exe', required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    exe = pathlib.Path(args.exe).resolve()
    root = pathlib.Path(args.output).resolve()
    root.mkdir(parents=True, exist_ok=False)
    (root/'downloads').mkdir()
    (root/'temp').mkdir()
    api_port, proxy_port = free_port(), free_port()
    fixture_data = bytes(range(256)) * (128 * 1024)
    fixture_ok = {'flaky': False}
    evidence = []

    class Fixture(http.server.BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'
        def log_message(self, *_):
            pass
        def handle(self):
            try:
                super().handle()
            except (ConnectionResetError, ConnectionAbortedError):
                pass
        def do_HEAD(self):
            self.serve(False)
        def do_GET(self):
            self.serve(True)
        def serve(self, body):
            if self.path.startswith('/flaky') and not fixture_ok['flaky']:
                self.send_response(503)
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            size = len(fixture_data) if 'slow' in self.path else 1024 * 1024
            start, end = 0, size - 1
            range_header = self.headers.get('Range')
            if range_header:
                found = re.fullmatch(r'bytes=(\d+)-(\d*)', range_header)
                if not found:
                    self.send_error(416)
                    return
                start = int(found[1])
                if found[2]:
                    end = min(end, int(found[2]))
                if start > end:
                    self.send_error(416)
                    return
            self.send_response(206 if range_header else 200)
            self.send_header('Content-Type', 'application/octet-stream')
            self.send_header('Content-Length', str(end-start+1))
            self.send_header('Accept-Ranges', 'bytes')
            self.send_header('ETag', '"localfix6-fixture-v1"')
            if range_header:
                self.send_header('Content-Range', f'bytes {start}-{end}/{size}')
            self.end_headers()
            if body:
                try:
                    for offset in range(start, end+1, 65536):
                        self.wfile.write(fixture_data[offset:min(offset+65536,end+1)])
                        self.wfile.flush()
                        if 'slow' in self.path:
                            time.sleep(0.04)
                except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                    pass

    fixture = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
    fixture.daemon_threads = True
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    fixture_url = f'http://127.0.0.1:{fixture.server_port}'
    base = f'http://127.0.0.1:{api_port}'
    config = '\n'.join([
        'workdir: '+json.dumps(str(root)),
        'api:', '  hostname: 127.0.0.1', f'  port: {api_port}',
        'proxy:', '  enabled: true', '  system: false', '  tun: false',
        '  skipInstallRootCert: true', '  hostname: 127.0.0.1', f'  port: {proxy_port}',
        'download:', '  dir: '+json.dumps(str(root/'downloads')),
        '  filenameTemplate: "{{filename}}"', '  playDoneAudio: false',
        '  maxRunning: 1', '  resourceConcurrency: 2', '  segmentConcurrency: 2',
        '  connectionConcurrency: 4', '  speedLimitMBps: 4',
        'db:', '  filepath: '+json.dumps(str(root/'data.db')),
        'mcp:', '  enabled: true', 'bridge:', '  enabled: false',
    ])
    (root/'config.yaml').write_text(config, encoding='utf-8')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    process = None
    stdout = (root/'stdout.log').open('ab')

    def record(name, **details):
        evidence.append({'check': name, 'passed': True, **details})
        print('PASS '+name, flush=True)

    def api(path, payload=None):
        request = urllib.request.Request(base+path,
            data=None if payload is None else json.dumps(payload).encode(),
            headers={'Content-Type':'application/json'})
        with opener.open(request, timeout=25) as response:
            result = json.load(response)
        assert result.get('code') == 0, result
        return result.get('data')

    def start():
        nonlocal process
        env = dict(os.environ, TMP=str(root/'temp'), TEMP=str(root/'temp'))
        process = subprocess.Popen([str(exe),'--config',str(root/'config.yaml')],
            cwd=root,env=env,stdin=subprocess.DEVNULL,stdout=stdout,stderr=stdout,
            creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0))
        status = wait_for(lambda: api('/api/status'), 'API startup')
        assert status['api']['listening'] and status['proxy']['listening'], status
        return status

    def row(task_id):
        with sqlite3.connect((root/'data.db').as_uri()+'?mode=ro',uri=True,timeout=3) as db:
            db.row_factory = sqlite3.Row
            found = db.execute('SELECT * FROM download_task WHERE id=?',(task_id,)).fetchone()
            return dict(found) if found else None

    def downloaded(task_id):
        with sqlite3.connect((root/'data.db').as_uri()+'?mode=ro',uri=True,timeout=3) as db:
            return db.execute('SELECT COALESCE(SUM(downloaded),0) FROM download_resource WHERE task_id=?',(task_id,)).fetchone()[0]

    def create(name, resource, auto=True):
        data = api('/api/v1/download_task/create_by_url', {'objects':[{
            'url':fixture_url+resource, 'filename':name, 'auto_start':auto}]})
        item = data['tasks'][0]
        assert item.get('code',0) == 0, item
        return item['data']['task']['id']

    def action(name, task_id, **extra):
        data=api('/api/v1/download_task/'+name, {'task_ids':[task_id],**extra})
        for result in data.get('results',[]):
            assert result.get('success'), result
        return data

    def finished(task_id):
        wait_for(lambda:row(task_id)['status']==5, f'task {task_id} finish',75)
        with sqlite3.connect((root/'data.db').as_uri()+'?mode=ro',uri=True) as db:
            resources=db.execute('SELECT download_dir,name FROM download_resource WHERE task_id=? AND deleted_at IS NULL',(task_id,)).fetchall()
        paths=[pathlib.Path(directory)/name for directory,name in resources]
        assert paths and all(p.is_file() for p in paths), paths
        return paths

    try:
        status=start()
        record('binary starts with isolated API and proxy',version=status['version'],api_port=api_port,proxy_port=proxy_port)
        with opener.open(base+'/',timeout=8) as response:
            html=response.read().decode('utf-8')
        (root/'served-index.html').write_text(html,encoding='utf-8')
        assets=[path for path in re.findall(r'(?:src|href)="([^\"]+)"',html)
                if path.startswith(('src/','public/','/__assets/','/favicon.ico'))]
        assert assets, 'No embedded UI assets'
        for path in sorted(set(assets)):
            with opener.open(urllib.parse.urljoin(base+'/',path),timeout=8) as response:
                assert response.status==200 and response.read(), path
        record('served UI and referenced assets',asset_count=len(set(assets)))
        conn=http.client.HTTPConnection('127.0.0.1',proxy_port,timeout=8)
        conn.request('GET',fixture_url+'/proxy-proof.bin')
        response=conn.getresponse()
        assert response.status==200 and response.read()==fixture_data[:1024*1024]
        conn.close()
        record('actual HTTP proxy forwards bytes correctly')
        task_id=create('multi\nline\tfixture.bin','/normal.bin',False)
        assert row(task_id)['status']==0
        action('start',task_id)
        paths=finished(task_id)
        assert paths[0].read_bytes()==fixture_data[:1024*1024]
        assert not any(c in paths[0].name for c in '\r\n\t')
        record('create, start, multiline filename and file hash',sha256=hashlib.sha256(paths[0].read_bytes()).hexdigest())
        task_id=create('pause-resume.bin','/slow-pause.bin')
        wait_for(lambda:downloaded(task_id)>131072,'download progress')
        competing_log=(root/'competing-instance.log').open('wb')
        duplicate=subprocess.Popen([str(exe),'--config',str(root/'config.yaml')],cwd=root,
            stdin=subprocess.DEVNULL,stdout=competing_log,stderr=competing_log,
            creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0))
        try:
            duplicate.wait(timeout=12)
        finally:
            if duplicate.poll() is None:
                duplicate.kill()
                duplicate.wait(timeout=5)
            competing_log.close()
        assert row(task_id)['status']==2, 'second instance changed running task state'
        assert 'already using this database' in (root/'competing-instance.log').read_text(encoding='utf-8',errors='replace')
        record('duplicate instance blocked before changing active task state')
        action('pause',task_id)
        assert row(task_id)['status']==3
        paused=downloaded(task_id)
        time.sleep(0.6)
        assert downloaded(task_id)==paused
        action('resume',task_id)
        paths=finished(task_id)
        assert paths[0].read_bytes()==fixture_data
        record('pause and resume preserve exact file bytes')
        task_id=create('cancel-active.bin','/slow-cancel.bin')
        wait_for(lambda:downloaded(task_id)>131072,'active delete progress')
        action('delete',task_id,delete_files=True)
        assert row(task_id)['deleted_at'] is not None
        assert not list((root/'downloads').glob('*cancel-active*'))
        record('delete running task and fixture file')
        task_id=create('retry.bin','/flaky.bin')
        wait_for(lambda:row(task_id)['status']==6,'failed state',40)
        fixture_ok['flaky']=True
        action('retry',task_id)
        assert finished(task_id)[0].read_bytes()==fixture_data[:1024*1024]
        record('failed HTTP task retries successfully')
        task_id=create('crash-recover.bin','/slow-crash.bin')
        wait_for(lambda:downloaded(task_id)>131072,'crash progress')
        process.kill()
        process.wait(timeout=10)
        prior_log=(root/'temp'/'wx_channels_download'/'app.log').read_bytes()
        start()
        assert row(task_id)['status']==3, row(task_id)
        assert (root/'temp'/'wx_channels_download'/'app.log').read_bytes().startswith(prior_log)
        action('resume',task_id)
        assert finished(task_id)[0].read_bytes()==fixture_data
        record('forced process exit, startup recovery, resume and log preservation')
        api('/api/service/stop?name=application',{})
        assert process.wait(timeout=20)==0
        record('graceful application stop')
        with sqlite3.connect((root/'data.db').as_uri()+'?mode=ro',uri=True) as db:
            assert db.execute('PRAGMA integrity_check').fetchone()[0]=='ok'
        record('test database integrity')
    finally:
        if process is not None and process.poll() is None:
            process.kill()
            process.wait(timeout=10)
        fixture.shutdown()
        stdout.close()
        (root/'verification.json').write_text(json.dumps(evidence,ensure_ascii=False,indent=2),encoding='utf-8')
    print(json.dumps({'passed':len(evidence),'output':str(root)},ensure_ascii=False),flush=True)


if __name__=='__main__':
    main()
