# BTUN 1.1 wire compatibility

This package is an independent server implementation for the wire format used
by the BTUN Android client. It does not load or execute an external protocol
server.

TCP begins with the 26-byte client hello. Optional cover/payload bytes may
precede it and are discarded up to the configured limit. The server returns
the 26-byte server hello. UDP sends each hello or frame in its own datagram.

After the hello, every packet has a six-byte header:

| Offset | Size | Meaning |
| --- | ---: | --- |
| 0 | 1 | packet type |
| 1 | 1 | flags, currently zero |
| 2 | 4 | unsigned payload length, big endian |
| 6 | N | payload |

Packet types:

| Type | Direction | Payload |
| ---: | --- | --- |
| 1 | client -> server | UTF-8 `username:password` |
| 2 | server -> client | acceptance byte (`1` or `0`) plus message |
| 3 | both | empty request; four-byte assigned IPv4 response |
| 4 | both | complete raw IPv4 packet |
| 5 | client -> server | empty keep-alive |

The server validates that every client data packet is IPv4 and that its source
matches the address assigned to that authenticated session. Internet-bound
packets are written to the TUN device; packets read from TUN are routed to the
session owning their destination address.
