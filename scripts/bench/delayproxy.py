#!/usr/bin/env python3
"""SSH ProxyCommand that adds a fixed one-way delay in each direction (RTT = 2 * delay).

usage: delayproxy.py <host> <port> <one-way-delay-ms>
"""
import heapq
import os
import socket
import sys
import threading
import time

host, port, delay = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]) / 1000.0
sock = socket.create_connection((host, port))
sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)


def pump(read, write, close):
    q = []  # (due, seq, data)
    cv = threading.Condition()
    done = [False]
    seq = [0]

    def reader():
        while True:
            try:
                data = read()
            except OSError:
                data = b""
            with cv:
                if not data:
                    done[0] = True
                    cv.notify()
                    return
                seq[0] += 1
                heapq.heappush(q, (time.monotonic() + delay, seq[0], data))
                cv.notify()

    def writer():
        while True:
            with cv:
                while not q and not done[0]:
                    cv.wait()
                if not q and done[0]:
                    close()
                    return
                due, _, data = q[0]
                now = time.monotonic()
                if due > now:
                    cv.wait(due - now)
                    continue
                heapq.heappop(q)
            try:
                write(data)
            except OSError:
                return

    threading.Thread(target=reader, daemon=True).start()
    t = threading.Thread(target=writer, daemon=True)
    t.start()
    return t


def stdout_write(b):
    os.write(1, b)


def sock_close():
    try:
        sock.shutdown(socket.SHUT_WR)
    except OSError:
        pass


t1 = pump(lambda: os.read(0, 65536), sock.sendall, sock_close)
t2 = pump(lambda: sock.recv(65536), stdout_write, lambda: os.close(1))
t2.join()
