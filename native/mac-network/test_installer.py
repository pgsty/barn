#!/usr/bin/env python3
"""Run the real installer body against an isolated fake filesystem/launchd.

Only this test copy bypasses uid checks. Every privileged destination and
launchctl/install/stat invocation is replaced; no real service is queried or
changed and the native vmnet helper is never started.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

ap = argparse.ArgumentParser()
ap.add_argument("--workdir", required=True)
args = ap.parse_args()
work = Path(args.workdir).resolve()
work.mkdir(parents=True, exist_ok=True)
installer = (Path(__file__).resolve().parent / "install.sh").read_text()
checks = []


def executable(path, body):
    path.write_text(body)
    path.chmod(0o700)
    return path


for case in ["idle-uninstall", "active-uninstall", "unregistered-idle", "unregistered-busy", "wrong-config", "missing-config", "launchctl-error", "same-binary-old-daemon", "already-current"]:
    with tempfile.TemporaryDirectory(prefix="installer-", dir=work) as temporary:
        root = Path(temporary)
        uid, ident = os.getuid(), "f60e8690-61aa-4a6f-b044-2a1ff03b4b2f"
        label = f"io.pgsty.farrow.mac-network.{uid}.{ident}"
        library, run, log = root / "Library", root / "run", root / "log"
        config_root = library / "Application Support/Farrow/mac-network" / str(uid) / ident
        config_root.mkdir(parents=True)
        (library / "PrivilegedHelperTools").mkdir()
        (library / "LaunchDaemons").mkdir()
        (run / "farrow-mac").mkdir(parents=True)
        (log / "farrow-mac").mkdir(parents=True)
        binary = library / "PrivilegedHelperTools/io.pgsty.farrow.mac-network"
        config_path = config_root / "config.json"
        plist = library / "LaunchDaemons" / (label + ".plist")
        log_path = log / "farrow-mac" / f"{uid}-{ident}.log"
        socket_path = run / "farrow-mac" / f"{uid}-{ident}.sock"
        other = library / "LaunchDaemons/io.pgsty.farrow.mac-network.other.plist"
        other.write_text("other installation must remain")
        log_path.write_text("retained diagnostic")
        plist.write_text("fixture plist")
        config = {"schema_version": 1, "uid": uid, "installation_id": ident, "subnet": "10.10.20.0/24", "slots": []}
        source_config = root / "source-config.json"
        source_config.write_text(json.dumps(config))
        config_path.write_text("different immutable config" if case == "wrong-config" else source_config.read_text())
        if case == "missing-config":
            config_path.unlink()
        state = root / "service-state"
        state.write_text("absent" if case.startswith("unregistered") else "running")
        running_build = root / "running-build"
        running_build.write_text("b" * 64 if case == "same-binary-old-daemon" else "a" * 64)
        calls = root / "launchctl-calls"
        helper = executable(root / "source-helper", '''#!/usr/bin/env python3
import fcntl,json,os,sys
from pathlib import Path
root=Path(os.environ['FMN_SIM_ROOT']); args=sys.argv[1:]; case=os.environ['FMN_SIM_CASE']
def emit(value): print(json.dumps(value),flush=True)
if args==['--version']: emit({'name':'farrow-mac-network','protocol':1,'build_id':'a'*64});sys.exit(0)
if args[0]=='validate-config':
 c=json.loads(Path(args[2]).read_text());c.update(ok=True,socket=os.environ['FMN_SIM_SOCKET'],label=os.environ['FMN_SIM_LABEL']);emit(c);sys.exit(0)
if args[0]=='status':
 if (root/'service-state').read_text()!='running': emit({'ok':False});sys.exit(69)
 emit({'ok':True,'connected':[case=='active-uninstall',False],'build_id':(root/'running-build').read_text()});sys.exit(0)
if args[0] in ['maintenance','repair-lock','admin-lock']:
 key={'maintenance':'maintenance_lease_held','repair-lock':'repair_lock_held','admin-lock':'admin_lock_held'}[args[0]]
 if args[0]=='repair-lock' and case=='unregistered-busy': emit({'ok':False});sys.exit(75)
 lockpath=root/('admin.lock' if args[0]=='admin-lock' else 'daemon.lock')
 with lockpath.open('a+') as held:
  try: fcntl.flock(held,fcntl.LOCK_EX|fcntl.LOCK_NB)
  except BlockingIOError: emit({'ok':False});sys.exit(75)
  emit({'ok':True,key:True});sys.stdin.buffer.read()
 sys.exit(0)
sys.exit(64)
''')
        binary.write_bytes(helper.read_bytes())
        binary.chmod(0o755)
        launchctl = executable(root / "launchctl", '''#!/usr/bin/env python3
import os,sys
from pathlib import Path
r=Path(os.environ['FMN_SIM_ROOT']);a=sys.argv[1:];label=os.environ['FMN_SIM_LABEL']
with (r/'launchctl-calls').open('a') as f:f.write(' '.join(a)+'\\n')
if a[0] in ['print','bootout'] and a[1]!='system/'+label:sys.exit(98)
if a[0]=='print':
 if os.environ['FMN_SIM_CASE']=='launchctl-error':sys.exit(1)
 if (r/'service-state').read_text()=='running':print('state = running\\npid = 99999');sys.exit(0)
 sys.exit(113)
if a[0]=='bootout':(r/'service-state').write_text('absent');sys.exit(0)
if a[0]=='bootstrap':(r/'service-state').write_text('running');(r/'running-build').write_text('a'*64);sys.exit(0)
sys.exit(97)
''')
        install = executable(root / "install", '''#!/usr/bin/env python3
import subprocess,sys
a=sys.argv[1:];clean=[];i=0
while i<len(a):
 if a[i] in ['-o','-g']:i+=2
 else:clean.append(a[i]);i+=1
sys.exit(subprocess.call(['/usr/bin/install']+clean))
''')
        stat = executable(root / "stat", '''#!/usr/bin/env python3
import os,subprocess,sys
if sys.argv[1:3]==['-f','%u']:print(os.getuid() if sys.argv[-1].endswith('.sock') else 0);sys.exit(0)
sys.exit(subprocess.call(['/usr/bin/stat']+sys.argv[1:]))
''')
        # Assert the test substitution surface before executing. No test copy
        # has a /Library, /var/run or /var/log write destination left.
        script = installer.replace('if [[ $EUID != 0 ]]; then', 'if false; then')
        for original, replacement in [("/Library", str(library)), ("/var/run", str(run)), ("/var/log", str(log)), ("/bin/launchctl", str(launchctl)), ("/usr/bin/install", str(install)), ("/usr/bin/stat", str(stat))]:
            script = script.replace(original, replacement)
        assert 'binary=/Library' not in script and 'ensure_directory /var/' not in script
        test_script = root / "installer.sh"
        test_script.write_text(script)
        environment = dict(os.environ, FMN_SIM_ROOT=str(root), FMN_SIM_CASE=case, FMN_SIM_LABEL=label, FMN_SIM_SOCKET=str(socket_path))
        environment.pop("SUDO_UID", None)
        command = ["/bin/bash", str(test_script)]
        if case not in ["same-binary-old-daemon", "already-current"]:
            command.append("--uninstall")
        command += ["--binary", str(helper), "--config", str(source_config)]
        result = subprocess.run(command, env=environment, capture_output=True, text=True, timeout=20)
        history = calls.read_text() if calls.exists() else ""
        expected = 0 if case in ["idle-uninstall", "unregistered-idle", "same-binary-old-daemon", "already-current"] else 65 if case in ["wrong-config", "missing-config"] else 75
        assert result.returncode == expected, (case, result.returncode, result.stdout, result.stderr)
        assert other.read_text() == "other installation must remain"
        assert binary.exists() and log_path.read_text() == "retained diagnostic"
        if case in ["idle-uninstall", "unregistered-idle"]:
            assert not config_root.exists() and not plist.exists()
            assert json.loads(result.stdout)["uninstalled"] is True
        elif case in ["same-binary-old-daemon", "already-current"]:
            assert config_path.exists() and plist.exists()
            assert ("bootout" in history) == (case == "same-binary-old-daemon")
            assert ("bootstrap" in history) == (case == "same-binary-old-daemon")
        else:
            assert "bootout" not in history and "bootstrap" not in history
            assert plist.exists()
        checks.append("installer-" + case)

print(json.dumps({"ok": True, "checks": checks, "count": len(checks), "real_system_changed": False}))
