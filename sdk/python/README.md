# dawnbx (Python)

Python SDK for dawnbx, self-hosted sandboxes for AI agents. No dependencies, Python 3.9+.

```python
from dawnbx import Sandbox

with Sandbox.create() as sb:  # killed when the block exits
    r = sb.exec("python -c 'print(1+1)'")
    print(r.exit_code, r.stdout)
    sb.files.write("notes.txt", "hi\n")
    kids = sb.fork(3)  # copies /workspace into 3 new sandboxes
```

Set `DAWNBX_URL` and `DAWNBX_API_KEY` (the installer prints both), or pass
`url=` / `api_key=`. Errors raise `DawnbxError` with a stable `code` and a `hint`.
