# Live end-to-end check against a running server (DAWNBX_URL, DAWNBX_API_KEY).
#   python tests/smoke.py
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent / "src"))
from dawnbx import DawnbxError, Sandbox

t0 = time.time()


def step(s):
    print(f"{time.time() - t0:5.1f}s  {s}")


sb = Sandbox.create(ttl="10m")
step(f"created {sb.id}")
try:
    r = sb.exec("python -c 'import sys; print(sys.version_info[:2])'; exit 7")
    assert r.exit_code == 7 and "(3, 12)" in r.stdout, r
    assert "gvisor" in sb.exec("dmesg | head -1").stdout.lower()
    step("exec + gVisor")

    sb.files.write("dir/a.txt", "héllo\n")
    assert sb.files.read("dir/a.txt") == "héllo\n"
    try:
        sb.files.read("nope.txt")
        raise AssertionError("missing file read")
    except DawnbxError as e:
        assert e.code == "file_not_found", e
    step("files")

    try:
        sb.exec("sleep 30", timeout=1)
        raise AssertionError("no timeout")
    except DawnbxError as e:
        assert e.code == "exec_timeout", e
    assert sb.exec("echo $FOO", env={"FOO": "bar"}).stdout == "bar\n"
    step("timeout + env")

    sb.files.write("notes.txt", "base\n")
    t = time.time()
    kids = sb.fork(2)
    step(f"fork(2) in {(time.time() - t) * 1000:.0f} ms")
    try:
        for i, k in enumerate(kids):
            k.exec(f"echo child-{i} >> notes.txt")
            assert k.files.read("notes.txt") == f"base\nchild-{i}\n"
            assert k.info.parent == sb.id
        assert sb.files.read("notes.txt") == "base\n"
    finally:
        for k in kids:
            k.kill()
    step("fork isolation")

    sb.extend(None)
    assert sb.refresh().expires_at is None
    assert any(s.id == sb.id for s in Sandbox.list())
    step("extend + list")
finally:
    sb.kill()
try:
    Sandbox.get(sb.id)
    raise AssertionError("still exists")
except DawnbxError as e:
    assert e.code == "not_found", e
step("killed")
print("SMOKE OK")
