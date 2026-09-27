# Local environment and tooling

- Development platform is macOS (arm64). Target is Mac and Linux only; Windows support was
  explicitly dropped when flagged. Confidence: 0.9
- Dev/test machines are Lima VMs, not k3d and not cloud: `hack/dev-vm.sh` creates a fresh VM,
  runs the installer inside it, and copies config back to the Mac. k3d was dropped once Lima
  worked. Confidence: 0.9
- Deploy to the VM is slow: run it in the background and poll rather than blocking. Confidence: 0.85
- For multi-node testing, run two Lima VMs (server + worker) and join with the real installer. Confidence: 0.8
- Web browsing goes through the `/browse` binary. Never use the Chrome MCP tool. Confidence: 0.95
- "Use my browser session" refers to the `agent-browser` skill (session-backed browsing), not
  the plain `/browse` binary. Load the skill explicitly before browsing in that case. Confidence: 0.6
- Long `sleep` chains and foreground waits can be blocked by hooks; use poll-until loops. Confidence: 0.8
- Shell heredocs (`cat > f <<EOF`, `cat >> f <<'EOF'`) silently produce empty files in this
  environment. Use the Write/Edit tools, or python3, for file content. Confidence: 0.95
- zsh does not word-split unquoted variables; use shell functions or arrays for command wrappers. Confidence: 0.8
- `/tmp` is wiped on VM restart; keep scratch inside the project or a persistent path. Confidence: 0.75
- `rtk` filters and compresses command output; use `rtk proxy <cmd>` when the exact bytes matter. Confidence: 0.7
- Long-running `kubectl exec` background processes must have their output redirected, or exec hangs. Confidence: 0.75
- Terminal-side effects should be done on real machines (Lima VMs, real probes) rather than in
  mocks, whenever a probe is affordable. Confidence: 0.8
