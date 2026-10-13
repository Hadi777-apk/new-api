import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import time
import uuid

ROOT = Path('/home/ubuntu/db-capacity-800-20261009')
BASE_APP_IMAGE = 'sha256:ba499bcdccdf7e00854c97fcc029fee86e069805064fbd4d7d6e61e70ccfb246'

def output(args, timeout=10):
    return subprocess.check_output(args, text=True, timeout=timeout).strip()

def inspect(name):
    return json.loads(output(['docker', 'inspect', name]))[0]

def sql(statement, timeout=8):
    return output(['docker', 'exec', 'new-api-postgres', 'psql', '-U', 'newapi', '-d', 'newapi', '-At', '-v', 'ON_ERROR_STOP=1', '-c', statement], timeout)

def wait_database(expected, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if sql('SHOW max_connections', min(8, max(1, deadline-time.monotonic()))) == str(expected):
                return
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
            pass
        time.sleep(.25)
    raise RuntimeError('PostgreSQL did not become ready at the expected capacity')

def restore_configuration(state):
    pg = inspect('new-api-postgres')
    assert pg['Id'] == state['postgres_id'], 'database container identity changed'
    assert any(Path(m['Source']).resolve() == Path(state['host_volume']) for m in pg['Mounts'])
    parent = Path(state['host_volume']).resolve()
    target = Path(state['auto_conf'])
    assert target.resolve().is_relative_to(parent) and not target.is_symlink()
    backup = (ROOT/'postgresql.auto.conf.before').read_bytes()
    assert hashlib.sha256(backup).hexdigest() == state['auto_conf_sha256']
    temp = target.parent/('postgresql.auto.conf.restore-'+uuid.uuid4().hex)
    with temp.open('xb') as handle:
        handle.write(backup)
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temp, state['file_mode'])
    os.chown(temp, state['file_uid'], state['file_gid'])
    os.replace(temp, target)
    subprocess.run(['docker', 'restart', '-t', '30', 'new-api-postgres'], check=True, stdout=subprocess.DEVNULL, timeout=45)
    wait_database(state['max_connections'])

def pause_application_when_drained():
    deadline = time.monotonic()+120
    while time.monotonic()<deadline:
        subprocess.run(['docker', 'pause', 'new-api'], check=True, stdout=subprocess.DEVNULL, timeout=8)
        try:
            api = int(sql("SELECT count(*) FROM api_image_tasks WHERE status='running'"))
            drawing = int(sql("SELECT count(*) FROM drawing_items WHERE status IN ('running','recovering')"))
            transactions = int(sql("SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid() AND xact_start IS NOT NULL"))
            if api == 0 and drawing == 0 and transactions == 0:
                return
        except Exception:
            subprocess.run(['docker', 'unpause', 'new-api'], check=True, stdout=subprocess.DEVNULL, timeout=8)
            raise
        subprocess.run(['docker', 'unpause', 'new-api'], check=True, stdout=subprocess.DEVNULL, timeout=8)
        time.sleep(1)
    raise RuntimeError('image work did not drain; no capacity change made')

def ensure_application_unpaused():
    if inspect('new-api')['State']['Paused']:
        subprocess.run(['docker','unpause','new-api'],check=True,stdout=subprocess.DEVNULL,timeout=8)

def main():
    mode = sys.argv[1]
    assert mode in ('inspect', 'raise', 'rollback')
    assert output(['hostname']) == 'a1-ubuntu-free'
    os.umask(0o077)
    ROOT.mkdir(mode=0o700, exist_ok=True)
    app = inspect('new-api')
    assert app['Image'] == BASE_APP_IMAGE, 'application must be at the known pre-capacity image'
    if mode == 'rollback':
        state = json.loads((ROOT/'restore-state.json').read_bytes())
        try:
            if not app['State']['Paused']:
                subprocess.run(['docker','pause','new-api'],check=True,stdout=subprocess.DEVNULL,timeout=8)
            restore_configuration(state)
        finally:
            ensure_application_unpaused()
        print(json.dumps({'rolled_back': True, 'max_connections': state['max_connections']}))
        return
    pg = inspect('new-api-postgres')
    data_dir = Path(sql('SHOW data_directory'))
    mount = next(m for m in pg['Mounts'] if data_dir.is_relative_to(Path(m['Destination'])))
    host_root = Path(mount['Source']).resolve()
    auto = host_root/data_dir.relative_to(Path(mount['Destination']))/'postgresql.auto.conf'
    assert auto.resolve().is_relative_to(host_root) and not auto.is_symlink()
    settings = json.loads(sql("SELECT json_agg(json_build_object('name',name,'setting',setting,'context',context,'pending_restart',pending_restart)) FROM pg_settings WHERE name IN ('max_connections','shared_buffers','work_mem','superuser_reserved_connections')"))
    env = dict(item.split('=',1) for item in app['Config']['Env'] if '=' in item)
    report = {'application_image': app['Image'], 'postgres_image': pg['Image'], 'settings': settings, 'log_dsn_separate': bool(env.get('LOG_SQL_DSN')), 'pool_env': {k:env.get(k) for k in ['SQL_MAX_OPEN_CONNS','SQL_MAX_IDLE_CONNS','SQL_MAX_LIFETIME']}}
    if mode == 'inspect':
        print(json.dumps(report))
        return
    assert sql('SHOW max_connections') == '100', 'unexpected database baseline'
    assert not (ROOT/'restore-state.json').exists(), 'capacity transaction already prepared'
    attributes = auto.stat()
    before = auto.read_bytes()
    (ROOT/'postgresql.auto.conf.before').write_bytes(before)
    state = {'postgres_id': pg['Id'], 'host_volume': str(host_root), 'auto_conf': str(auto), 'auto_conf_sha256': hashlib.sha256(before).hexdigest(), 'max_connections': 100, 'file_uid': attributes.st_uid, 'file_gid': attributes.st_gid, 'file_mode': stat.S_IMODE(attributes.st_mode)}
    (ROOT/'restore-state.json').write_text(json.dumps(state,indent=2))
    (ROOT/'before.json').write_text(json.dumps(report,indent=2))
    with (ROOT/'production-before.dump').open('xb') as handle:
        subprocess.run(['docker','exec','new-api-postgres','pg_dump','-U','newapi','-d','newapi','-Fc'],stdout=handle,check=True,timeout=60)
    with (ROOT/'production-before.dump').open('rb') as handle:
        subprocess.run(['docker','exec','-i','new-api-postgres','pg_restore','--list'],stdin=handle,stdout=subprocess.DEVNULL,check=True,timeout=20)
    configuration_attempted = False
    try:
        pause_application_when_drained()
        configuration_attempted = True
        sql("ALTER SYSTEM SET max_connections='1000'")
        subprocess.run(['docker','restart','-t','30','new-api-postgres'],check=True,stdout=subprocess.DEVNULL,timeout=45)
        wait_database(1000)
    except Exception:
        if configuration_attempted:
            restore_configuration(state)
        raise
    finally:
        ensure_application_unpaused()
    report.update(max_connections_after=1000, maintenance_pause_used=True, backup_sha256=hashlib.sha256((ROOT/'production-before.dump').read_bytes()).hexdigest())
    (ROOT/'database-capacity-after.json').write_text(json.dumps(report,indent=2))
    print(json.dumps(report))

if __name__ == '__main__':
    main()
