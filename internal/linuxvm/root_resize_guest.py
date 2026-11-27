# Invoked over instance-bound SSH. No persistent guest helper is installed.
import json
import os
import re
import shutil
import subprocess
import sys


def run(*args, allowed=(0,)):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=dict(os.environ, LC_ALL="C"))
    if result.returncode not in allowed:
        raise RuntimeError("%s failed: %s" % (args[0], (result.stderr or result.stdout)[-1500:].strip()))
    return result.stdout.strip(), result.returncode


def read(path):
    with open(path, encoding="utf-8") as handle:
        return handle.read().strip()


def filesystem_bytes(device, filesystem):
    if filesystem == "ext4":
        output, _ = run("tune2fs", "-l", device)
        count = re.search(r"^Block count:\s+(\d+)$", output, re.M)
        size = re.search(r"^Block size:\s+(\d+)$", output, re.M)
    else:
        output, _ = run("xfs_info", "/")
        line = next((line for line in output.splitlines() if line.startswith("data")), "")
        count = re.search(r"\bblocks=(\d+)", line)
        size = re.search(r"\bbsize=(\d+)", line)
    if count is None or size is None:
        raise RuntimeError("cannot read root filesystem geometry")
    return int(count.group(1)) * int(size.group(1))


def probe(serial, target=0):
    for tool in ("findmnt", "blkid", "sfdisk", "growpart"):
        if not shutil.which(tool):
            raise RuntimeError("root growth requires " + tool)
    number, _ = run("findmnt", "-n", "-o", "MAJ:MIN", "--target", "/")
    filesystem, _ = run("findmnt", "-n", "-o", "FSTYPE", "--target", "/")
    options, _ = run("findmnt", "-n", "-o", "OPTIONS", "--target", "/")
    if "rw" not in options.split(",") or filesystem not in ("ext4", "xfs"):
        raise RuntimeError("root growth requires a writable ext4 or XFS root")
    for tool in (("resize2fs", "tune2fs") if filesystem == "ext4" else ("xfs_growfs", "xfs_info")):
        if not shutil.which(tool):
            raise RuntimeError("root growth requires " + tool)
    path = os.path.realpath("/sys/dev/block/" + number)
    if not os.path.isfile(path + "/partition"):
        raise RuntimeError("root must be a plain partition; LVM and encrypted roots are unsupported")
    parent = os.path.dirname(path)
    device = "/dev/" + os.path.basename(path)
    disk = "/dev/" + os.path.basename(parent)
    actual_serial = read(parent + "/serial")
    if actual_serial != serial or os.listdir(parent + "/slaves"):
        raise RuntimeError("root device does not match the Barn root disk serial")
    if int(read(parent + "/queue/logical_block_size")) != 512:
        raise RuntimeError("root growth requires 512-byte logical sectors")
    partition = int(read(path + "/partition"))
    start = int(read(path + "/start")) * 512
    part_bytes = int(read(path + "/size")) * 512
    disk_bytes = int(read(parent + "/size")) * 512
    table = json.loads(run("sfdisk", "--json", disk)[0])["partitiontable"]
    if table["label"] not in ("gpt", "dos") or table.get("sectorsize", 512) != 512:
        raise RuntimeError("unsupported root partition table")
    if table["label"] == "dos" and target > 512 * 2**32:
        raise RuntimeError("MBR root target exceeds the 2 TiB addressing limit")
    if table["label"] == "dos" and partition > 4:
        raise RuntimeError("logical MBR root partitions are unsupported")
    entries = table["partitions"]
    root_entries = [p for p in entries if os.path.realpath(p["node"]) == device]
    if len(root_entries) != 1 or root_entries[0]["start"] * 512 != start or root_entries[0]["size"] * 512 != part_bytes:
        raise RuntimeError("kernel and on-disk root partition geometry disagree")
    for other in entries:
        if os.path.realpath(other["node"]) != device and (other["start"] + other["size"]) * 512 > start:
            raise RuntimeError("root must be the last physical partition without overlapping partitions")
    uuid = run("blkid", "-s", "UUID", "-o", "value", device)[0]
    part_uuid = run("blkid", "-s", "PARTUUID", "-o", "value", device)[0]
    if not uuid or not part_uuid:
        raise RuntimeError("root UUID or PARTUUID is missing")
    result = dict(serial=serial, partition=partition, start=start, uuid=uuid,
                  part_uuid=part_uuid, filesystem=filesystem, disk_bytes=disk_bytes,
                  part_bytes=part_bytes, fs_bytes=filesystem_bytes(device, filesystem))
    return result, disk, device


def check_identity(actual, expected):
    for key in ("serial", "partition", "start", "uuid", "part_uuid", "filesystem"):
        if actual[key] != expected[key]:
            raise RuntimeError("root identity changed: " + key)
    for key in ("disk_bytes", "part_bytes", "fs_bytes"):
        if actual[key] < expected[key]:
            raise RuntimeError("root capacity decreased: " + key)


def verify(actual, expected, target):
    check_identity(actual, expected)
    if actual["disk_bytes"] != target:
        raise RuntimeError("guest root disk capacity does not match the target")
    tail = target - actual["start"] - actual["part_bytes"]
    if tail < 0 or tail > 2 * 1024 * 1024:
        raise RuntimeError("root partition does not fill the enlarged disk")
    slack = actual["part_bytes"] - actual["fs_bytes"]
    tolerance = 4096 if actual["filesystem"] == "ext4" else 16 * 1024 * 1024
    if slack < 0 or slack > tolerance or actual["fs_bytes"] < expected["fs_bytes"]:
        raise RuntimeError("root filesystem did not grow to fill the partition")


def main():
    mode, serial, target_text, expected_text = sys.argv[1:]
    target = int(target_text)
    actual, disk, device = probe(serial, target)
    if mode == "preflight":
        if target <= actual["disk_bytes"] or target % 512:
            raise RuntimeError("root target must exceed the current disk and align to 512 bytes")
        if actual["fs_bytes"] <= 0 or actual["fs_bytes"] > actual["part_bytes"]:
            raise RuntimeError("invalid root filesystem capacity")
    elif mode == "grow":
        expected = json.loads(expected_text)
        check_identity(actual, expected)
        if actual["disk_bytes"] != target:
            raise RuntimeError("guest root disk capacity does not match the target")
        output, code = run("growpart", "--fudge", "0", disk, str(actual["partition"]), allowed=(0, 1))
        if code == 1 and not output.startswith("NOCHANGE:"):
            raise RuntimeError("growpart did not confirm a no-op: " + output)
        if actual["filesystem"] == "ext4":
            run("resize2fs", device)
        else:
            run("xfs_growfs", "/")
        run("sync")
        actual, _, _ = probe(serial, target)
        verify(actual, expected, target)
    else:
        raise RuntimeError("unknown root resize operation")
    print(json.dumps(actual, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError) as error:
        sys.exit("root resize: " + str(error))
