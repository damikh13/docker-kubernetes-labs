#!/usr/bin/env python3
"""Tiny Prometheus exporter: reads cgroup v2 files of Docker containers directly.

For every container it finds (via the Docker socket) it reads the files you used by hand in part 3:
  memory.current, memory.max, memory.events, memory.stat, cpu.stat, cpu.max, pids.current, pids.max
and exposes them at :9100/metrics with the label name="<container name>".
"""
import http.client
import http.server
import json
import os
import socket

CGROUP_ROOT = os.environ.get("CGROUP_ROOT", "/host-cgroup")
DOCKER_SOCK = os.environ.get("DOCKER_SOCK", "/var/run/docker.sock")
PORT = int(os.environ.get("PORT", "9100"))


class UnixHTTPConnection(http.client.HTTPConnection):
    """HTTP over a unix socket (that is how the Docker API is reachable)."""

    def __init__(self, path):
        super().__init__("localhost")
        self.unix_path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(5)
        self.sock.connect(self.unix_path)


def docker_get(path):
    conn = UnixHTTPConnection(DOCKER_SOCK)
    try:
        conn.request("GET", path)
        return json.loads(conn.getresponse().read())
    finally:
        conn.close()


def cgroup_dir(cid):
    # systemd cgroup driver (your VM) first, cgroupfs driver as a fallback
    for p in (f"{CGROUP_ROOT}/system.slice/docker-{cid}.scope", f"{CGROUP_ROOT}/docker/{cid}"):
        if os.path.isdir(p):
            return p
    return None


def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return None


def read_kv(path):
    """Parse files like cpu.stat: 'key value' per line."""
    out = {}
    text = read(path)
    if text:
        for line in text.splitlines():
            key, _, val = line.partition(" ")
            try:
                out[key] = int(val)
            except ValueError:
                pass
    return out


def num(v):
    """'max' (no limit) and missing files become None."""
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def collect():
    metrics = {}  # metric name -> [type, help, [(container name, value)]]

    def emit(name, typ, help_, cname, value):
        if value is None:
            return
        metrics.setdefault(name, [typ, help_, []])[2].append((cname, value))

    try:
        containers = docker_get("/containers/json?all=1")
        ok = 1
    except Exception:
        containers, ok = [], 0
    metrics["cgroup_exporter_docker_ok"] = ["gauge", "1 if the Docker API answered", [("", ok)]]

    for c in containers:
        cid = c["Id"]
        name = c["Names"][0].lstrip("/")
        try:
            info = docker_get(f"/containers/{cid}/json")
            emit("docker_container_restart_count", "gauge", "Docker's automatic restart counter", name, info.get("RestartCount", 0))
            emit("docker_container_running", "gauge", "1 if the container is running", name, 1 if info["State"]["Running"] else 0)
        except Exception:
            pass

        d = cgroup_dir(cid)
        if not d:
            continue

        cur = num(read(d + "/memory.current"))
        stat = read_kv(d + "/memory.stat")
        emit("cgroup_memory_current_bytes", "gauge", "memory.current", name, cur)
        if cur is not None:
            emit("cgroup_memory_working_set_bytes", "gauge", "memory.current minus inactive file cache", name, cur - stat.get("inactive_file", 0))
        emit("cgroup_memory_max_bytes", "gauge", "memory.max (limit; absent if unlimited)", name, num(read(d + "/memory.max")))
        ev = read_kv(d + "/memory.events")
        emit("cgroup_memory_oom_kill_total", "counter", "memory.events oom_kill", name, ev.get("oom_kill"))
        emit("cgroup_memory_limit_hits_total", "counter", "memory.events max (times the limit was hit)", name, ev.get("max"))

        cpu = read_kv(d + "/cpu.stat")
        if "usage_usec" in cpu:
            emit("cgroup_cpu_usage_seconds_total", "counter", "cpu.stat usage_usec", name, cpu["usage_usec"] / 1e6)
        emit("cgroup_cpu_periods_total", "counter", "cpu.stat nr_periods", name, cpu.get("nr_periods"))
        emit("cgroup_cpu_throttled_periods_total", "counter", "cpu.stat nr_throttled", name, cpu.get("nr_throttled"))
        if "throttled_usec" in cpu:
            emit("cgroup_cpu_throttled_seconds_total", "counter", "cpu.stat throttled_usec", name, cpu["throttled_usec"] / 1e6)
        parts = (read(d + "/cpu.max") or "").split()
        if len(parts) == 2 and num(parts[0]) is not None and num(parts[1]):
            emit("cgroup_cpu_quota_cores", "gauge", "cpu.max quota / period (cores)", name, int(parts[0]) / int(parts[1]))

        emit("cgroup_pids_current", "gauge", "pids.current", name, num(read(d + "/pids.current")))
        emit("cgroup_pids_max", "gauge", "pids.max (absent if unlimited)", name, num(read(d + "/pids.max")))

    lines = []
    for mname in sorted(metrics):
        typ, help_, samples = metrics[mname]
        lines.append(f"# HELP {mname} {help_}")
        lines.append(f"# TYPE {mname} {typ}")
        for cname, value in samples:
            label = f'{{name="{cname}"}}' if cname else ""
            lines.append(f"{mname}{label} {value}")
    return "\n".join(lines) + "\n"


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_error(404)
            return
        body = collect().encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
