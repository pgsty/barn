import json
import subprocess
import sys
import time


def reply(value):
    print(json.dumps(value), flush=True)


def realtime_ns():
    # EL8 ships Python 3.6. Float-second APIs retain sub-microsecond resolution
    # at current epochs, below the uncertainty of an SSH clock measurement.
    return int(time.time() * 1_000_000_000)


def monotonic_ns():
    return int(time.monotonic() * 1_000_000_000)


def ntp_status():
    result = {"synchronized": None, "service": "unknown", "source": ""}
    try:
        output = subprocess.run(
            ["timedatectl", "show", "-p", "NTPSynchronized", "-p", "NTP"],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            universal_newlines=True, timeout=2,
        )
        fields = dict(line.split("=", 1) for line in output.stdout.splitlines() if "=" in line)
        if "NTPSynchronized" in fields:
            result["synchronized"] = fields["NTPSynchronized"] == "yes"
        if "NTP" in fields:
            result["service"] = "active" if fields["NTP"] == "yes" else "inactive"
    except (OSError, subprocess.TimeoutExpired):
        pass
    try:
        output = subprocess.run(
            ["chronyc", "-n", "tracking"], stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, timeout=2,
        )
        if output.returncode == 0:
            fields = dict((key.strip(), value.strip()) for key, value in
                          (line.split(":", 1) for line in output.stdout.splitlines() if ":" in line))
            result["service"] = "chrony"
            result["source"] = fields.get("Reference ID", "")
            result["synchronized"] = fields.get("Leap status") == "Normal"
    except (OSError, subprocess.TimeoutExpired):
        pass
    return result


def main():
    expected = json.loads(sys.argv[1])
    try:
        with open("/var/lib/barn/ready.json", encoding="utf-8") as stream:
            marker = json.load(stream)
    except FileNotFoundError:
        raise ValueError("guest setup is not complete")
    if any(marker.get(key) != value for key, value in expected.items()):
        raise ValueError("guest identity does not match the applied node")
    reply({"ready": True, "ntp": ntp_status()})
    samples = {}
    for line in sys.stdin:
        request = json.loads(line)
        if request["op"] == "sample":
            monotonic = monotonic_ns()
            stamp = realtime_ns()
            samples[monotonic] = stamp
            reply({"unix_ns": stamp, "monotonic_ns": monotonic})
        elif request["op"] == "set":
            anchor = request["monotonic_ns"]
            if anchor not in samples:
                raise ValueError("clock sample is unknown")
            elapsed = monotonic_ns() - anchor
            if not 0 <= elapsed <= 5_000_000_000:
                raise ValueError("clock sample expired before adjustment")
            if abs(realtime_ns() - samples[anchor] - elapsed) > 100_000_000:
                raise ValueError("guest clock changed during measurement; retry")
            # Carry the sampled host epoch forward on the guest monotonic clock,
            # so time spent transporting this request cannot set a stale epoch.
            time.clock_settime(time.CLOCK_REALTIME, (request["unix_ns"] + elapsed) / 1_000_000_000)
            samples.clear()
            reply({"adjusted": True})
        else:
            raise ValueError("unknown clock operation")


try:
    main()
except Exception as error:
    reply({"error": str(error)})
    sys.exit(1)
