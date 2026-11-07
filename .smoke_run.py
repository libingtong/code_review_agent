import os, pty, time, select, fcntl, termios, struct, signal, re, sys

DURATION = int(sys.argv[1]) if len(sys.argv) > 1 else 180
cmd = ["./run.sh", "-config", "config.smoke.yaml", "-dir", "sample_vuln_app"]
pid, fd = pty.fork()
if pid == 0:
    time.sleep(0.4)
    os.environ["TERM"] = "xterm-256color"
    os.environ["COLORTERM"] = "truecolor"
    os.execv(cmd[0], cmd)

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 50, 160, 0, 0))
raw = bytearray()
deadline = time.time() + DURATION
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.25)
    if r:
        try:
            chunk = os.read(fd, 65536)
        except OSError:
            break
        if not chunk:
            break
        raw += chunk
        if b"\x1b]10;?" in chunk:
            os.write(fd, b"\x1b]10;rgb:ffff/ffff/ffff\x1b\\")
        if b"\x1b]11;?" in chunk:
            os.write(fd, b"\x1b]11;rgb:0000/0000/0000\x1b\\")
        if b"\x1b[6n" in chunk:
            os.write(fd, b"\x1b[1;1R")
        open(".smoke_raw.bin", "wb").write(raw)
        tail = raw[-20000:].decode("utf-8", "replace")
        if "全部审计 Agent 已完成" in tail or "本轮已停止" in tail:
            break

os.write(fd, b"\x03")
time.sleep(1.5)
try:
    os.kill(pid, signal.SIGKILL)
except ProcessLookupError:
    pass
try:
    os.waitpid(pid, 0)
except ChildProcessError:
    pass

text = raw.decode("utf-8", "replace")
text = re.sub(r"\x1b\][^\x07\x1b]*(\x07|\x1b\\)", "", text)
text = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", text)
text = re.sub(r"\x1b[()][A-Z0-9]", "", text)
text = text.replace("\r", "")
open(".smoke_capture.txt", "w").write(text)
print("BYTES", len(raw), "TEXT", len(text))
