#!/usr/bin/env python3
"""Live BHP1 registration and OpenSSH-banner smoke test."""

import argparse
import hashlib
import os
import socket
import struct


def crypt(data, sid, mode, sequence, response):
    seed = sid + bytes((mode,)) + struct.pack(">Q", sequence) + bytes((response,))
    result = bytearray(len(data))
    for offset in range(0, len(data), 32):
        key = hashlib.sha256(
            seed + struct.pack(">I", offset // 32)
        ).digest()
        part = data[offset:offset + 32]
        for index, value in enumerate(part):
            result[offset + index] = value ^ key[index]
    return bytes(result)


def read_exact(connection, size):
    result = bytearray()
    while len(result) < size:
        chunk = connection.recv(size - len(result))
        if not chunk:
            raise EOFError("truncated BHTTP response")
        result.extend(chunk)
    return bytes(result)


def response(connection):
    status, size = struct.unpack(">BI", read_exact(connection, 5))
    return status, read_exact(connection, size) if size else b""


def request(host, port, sid, mode, sequence, payload=b"", value=None):
    if value is None:
        value = len(payload)
    connection = socket.create_connection((host, port), timeout=5)
    connection.settimeout(5)
    connection.sendall(
        struct.pack(">B16sQI", mode, sid, sequence, value) + payload
    )
    return connection


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    args = parser.parse_args()
    sid = os.urandom(16)

    with request(args.host, args.port, sid, 1, 0) as connection:
        status, _ = response(connection)
    if status != 0:
        raise SystemExit("registration failed with status {}".format(status))

    with request(args.host, args.port, sid, 2, 0, value=65536) as connection:
        status, body = response(connection)
    if status != 2 or len(body) < 4:
        raise SystemExit("download failed with status {}".format(status))
    size = struct.unpack_from(">I", body, 0)[0]
    banner = crypt(body[4:4 + size], sid, 2, 0, 1)
    if not banner.startswith(b"SSH-"):
        raise SystemExit("invalid SSH banner: {!r}".format(banner[:80]))

    identification = b"SSH-2.0-Bilola-Go-Smoke\r\n"
    encrypted = crypt(identification, sid, 1, 0, 0)
    with request(args.host, args.port, sid, 1, 0, encrypted) as connection:
        status, _ = response(connection)
    if status != 0:
        raise SystemExit("upload failed with status {}".format(status))

    print(
        "BHP1 registration, SSH banner and upload OK on {}:{} ({!r})".format(
            args.host, args.port, banner.rstrip()
        )
    )


if __name__ == "__main__":
    main()
